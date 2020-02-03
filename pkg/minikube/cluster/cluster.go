/*
Copyright 2016 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cluster

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"math"
	"net"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/docker/machine/libmachine"
	"github.com/docker/machine/libmachine/drivers"
	"github.com/docker/machine/libmachine/engine"
	"github.com/docker/machine/libmachine/host"
	"github.com/docker/machine/libmachine/mcnerror"
	"github.com/docker/machine/libmachine/provision"
	"github.com/docker/machine/libmachine/ssh"
	"github.com/docker/machine/libmachine/state"
	"github.com/golang/glog"
	"github.com/juju/mutex"
	"github.com/pkg/errors"
	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/disk"
	"github.com/shirou/gopsutil/mem"
	"github.com/spf13/viper"

	"k8s.io/minikube/pkg/drivers/kic"
	"k8s.io/minikube/pkg/drivers/kic/oci"
	"k8s.io/minikube/pkg/minikube/command"
	"k8s.io/minikube/pkg/minikube/config"
	"k8s.io/minikube/pkg/minikube/constants"
	"k8s.io/minikube/pkg/minikube/driver"
	"k8s.io/minikube/pkg/minikube/exit"
	"k8s.io/minikube/pkg/minikube/localpath"
	"k8s.io/minikube/pkg/minikube/out"
	"k8s.io/minikube/pkg/minikube/registry"
	"k8s.io/minikube/pkg/minikube/sshutil"
	"k8s.io/minikube/pkg/minikube/vmpath"
	"k8s.io/minikube/pkg/util/lock"
	"k8s.io/minikube/pkg/util/retry"
)

// hostRunner is a minimal host.Host based interface for running commands
type hostRunner interface {
	RunSSHCommand(string) (string, error)
}

var (
	// The maximum the guest VM clock is allowed to be ahead and behind. This value is intentionally
	// large to allow for inaccurate methodology, but still small enough so that certificates are likely valid.
	maxClockDesyncSeconds = 2.1

	// requiredDirectories are directories to create on the host during setup
	requiredDirectories = []string{
		vmpath.GuestAddonsDir,
		vmpath.GuestManifestsDir,
		vmpath.GuestEphemeralDir,
		vmpath.GuestPersistentDir,
		vmpath.GuestCertsDir,
		path.Join(vmpath.GuestPersistentDir, "images"),
		path.Join(vmpath.GuestPersistentDir, "binaries"),
	}
)

// This init function is used to set the logtostderr variable to false so that INFO level log info does not clutter the CLI
// INFO lvl logging is displayed due to the kubernetes api calling flag.Set("logtostderr", "true") in its init()
// see: https://github.com/kubernetes/kubernetes/blob/master/pkg/kubectl/util/logs/logs.go#L32-L34
func init() {
	if err := flag.Set("logtostderr", "false"); err != nil {
		exit.WithError("unable to set logtostderr", err)
	}

	// Setting the default client to native gives much better performance.
	ssh.SetDefaultClient(ssh.Native)
}

// CacheISO downloads and caches ISO.
func CacheISO(cfg config.MachineConfig) error {
	if driver.BareMetal(cfg.VMDriver) {
		return nil
	}
	return cfg.Downloader.CacheMinikubeISOFromURL(cfg.MinikubeISO)
}

// StartHost starts a host VM.
func StartHost(api libmachine.API, cfg config.MachineConfig) (*host.Host, error) {
	// Prevent machine-driver boot races, as well as our own certificate race
	releaser, err := acquireMachinesLock(cfg.Name)
	if err != nil {
		return nil, errors.Wrap(err, "boot lock")
	}
	start := time.Now()
	defer func() {
		glog.Infof("releasing machines lock for %q, held for %s", cfg.Name, time.Since(start))
		releaser.Release()
	}()

	exists, err := api.Exists(cfg.Name)
	if err != nil {
		return nil, errors.Wrapf(err, "exists: %s", cfg.Name)
	}
	if !exists {
		glog.Infoln("Machine does not exist... provisioning new machine")
		glog.Infof("Provisioning machine with config: %+v", cfg)
		return createHost(api, cfg)
	}

	glog.Infoln("Skipping create...Using existing machine configuration")

	h, err := api.Load(cfg.Name)
	if err != nil {
		return nil, errors.Wrap(err, "Error loading existing host. Please try running [minikube delete], then run [minikube start] again.")
	}

	if exists && cfg.Name == constants.DefaultMachineName {
		out.T(out.Tip, "Tip: Use 'minikube start -p <name>' to create a new cluster, or 'minikube delete' to delete this one.")
	}

	s, err := h.Driver.GetState()
	glog.Infoln("Machine state: ", s)
	if err != nil {
		return nil, errors.Wrap(err, "Error getting state for host")
	}

	if s == state.Running {
		out.T(out.Running, `Using the running {{.driver_name}} "{{.profile_name}}" VM ...`, out.V{"driver_name": cfg.VMDriver, "profile_name": cfg.Name})
	} else {
		out.T(out.Restarting, `Starting existing {{.driver_name}} VM for "{{.profile_name}}" ...`, out.V{"driver_name": cfg.VMDriver, "profile_name": cfg.Name})
		if err := h.Driver.Start(); err != nil {
			return nil, errors.Wrap(err, "start")
		}
		if err := api.Save(h); err != nil {
			return nil, errors.Wrap(err, "save")
		}
	}

	e := engineOptions(cfg)
	glog.Infof("engine options: %+v", e)

	out.T(out.Waiting, "Waiting for the host to be provisioned ...")
	err = configureHost(h, e)
	if err != nil {
		return nil, err
	}
	return h, nil
}

// acquireMachinesLock protects against code that is not parallel-safe (libmachine, cert setup)
func acquireMachinesLock(name string) (mutex.Releaser, error) {
	spec := lock.PathMutexSpec(filepath.Join(localpath.MiniPath(), "machines"))
	// NOTE: Provisioning generally completes within 60 seconds
	spec.Timeout = 10 * time.Minute

	glog.Infof("acquiring machines lock for %s: %+v", name, spec)
	start := time.Now()
	r, err := mutex.Acquire(spec)
	if err == nil {
		glog.Infof("acquired machines lock for %q in %s", name, time.Since(start))
	}
	return r, err
}

// configureHost handles any post-powerup configuration required
func configureHost(h *host.Host, e *engine.Options) error {
	start := time.Now()
	glog.Infof("configureHost: %+v", h.Driver)
	defer func() {
		glog.Infof("configureHost completed within %s", time.Since(start))
	}()

	if err := createRequiredDirectories(h); err != nil {
		errors.Wrap(err, "required directories")
	}

	if len(e.Env) > 0 {
		h.HostOptions.EngineOptions.Env = e.Env
		glog.Infof("Detecting provisioner ...")
		provisioner, err := provision.DetectProvisioner(h.Driver)
		if err != nil {
			return errors.Wrap(err, "detecting provisioner")
		}
		glog.Infof("Provisioning with %s: %+v", provisioner.String(), *h.HostOptions)
		if err := provisioner.Provision(*h.HostOptions.SwarmOptions, *h.HostOptions.AuthOptions, *h.HostOptions.EngineOptions); err != nil {
			return errors.Wrap(err, "provision")
		}
	}

	if driver.BareMetal(h.Driver.DriverName()) {
		glog.Infof("%s is a local driver, skipping auth/time setup", h.Driver.DriverName())
		return nil
	}
	glog.Infof("Configuring auth for driver %s ...", h.Driver.DriverName())
	if err := h.ConfigureAuth(); err != nil {
		return &retry.RetriableError{Err: errors.Wrap(err, "Error configuring auth on host")}
	}
	return ensureSyncedGuestClock(h)
}

// ensureGuestClockSync ensures that the guest system clock is relatively in-sync
func ensureSyncedGuestClock(h hostRunner) error {
	d, err := guestClockDelta(h, time.Now())
	if err != nil {
		glog.Warningf("Unable to measure system clock delta: %v", err)
		return nil
	}
	if math.Abs(d.Seconds()) < maxClockDesyncSeconds {
		glog.Infof("guest clock delta is within tolerance: %s", d)
		return nil
	}
	if err := adjustGuestClock(h, time.Now()); err != nil {
		return errors.Wrap(err, "adjusting system clock")
	}
	return nil
}

// guestClockDelta returns the approximate difference between the host and guest system clock
// NOTE: This does not currently take into account ssh latency.
func guestClockDelta(h hostRunner, local time.Time) (time.Duration, error) {
	out, err := h.RunSSHCommand("date +%s.%N")
	if err != nil {
		return 0, errors.Wrap(err, "get clock")
	}
	glog.Infof("guest clock: %s", out)
	ns := strings.Split(strings.TrimSpace(out), ".")
	secs, err := strconv.ParseInt(strings.TrimSpace(ns[0]), 10, 64)
	if err != nil {
		return 0, errors.Wrap(err, "atoi")
	}
	nsecs, err := strconv.ParseInt(strings.TrimSpace(ns[1]), 10, 64)
	if err != nil {
		return 0, errors.Wrap(err, "atoi")
	}
	// NOTE: In a synced state, remote is a few hundred ms ahead of local
	remote := time.Unix(secs, nsecs)
	d := remote.Sub(local)
	glog.Infof("Guest: %s Remote: %s (delta=%s)", remote, local, d)
	return d, nil
}

// adjustSystemClock adjusts the guest system clock to be nearer to the host system clock
func adjustGuestClock(h hostRunner, t time.Time) error {
	out, err := h.RunSSHCommand(fmt.Sprintf("sudo date -s @%d", t.Unix()))
	glog.Infof("clock set: %s (err=%v)", out, err)
	return err
}

// trySSHPowerOff runs the poweroff command on the guest VM to speed up deletion
func trySSHPowerOff(h *host.Host) error {
	s, err := h.Driver.GetState()
	if err != nil {
		glog.Warningf("unable to get state: %v", err)
		return err
	}
	if s != state.Running {
		glog.Infof("host is in state %s", s)
		return nil
	}

	out.T(out.Shutdown, `Powering off "{{.profile_name}}" via SSH ...`, out.V{"profile_name": h.Name})
	out, err := h.RunSSHCommand("sudo poweroff")
	// poweroff always results in an error, since the host disconnects.
	glog.Infof("poweroff result: out=%s, err=%v", out, err)
	return nil
}

// StopHost stops the host VM, saving state to disk.
func StopHost(api libmachine.API) error {
	glog.Infof("Stopping host ...")
	start := time.Now()
	defer func() {
		glog.Infof("Stopped host within %s", time.Since(start))
	}()

	machineName := viper.GetString(config.MachineProfile)
	host, err := api.Load(machineName)
	if err != nil {
		return errors.Wrapf(err, "load")
	}

	out.T(out.Stopping, `Stopping "{{.profile_name}}" in {{.driver_name}} ...`, out.V{"profile_name": machineName, "driver_name": host.DriverName})
	if host.DriverName == driver.HyperV {
		glog.Infof("As there are issues with stopping Hyper-V VMs using API, trying to shut down using SSH")
		if err := trySSHPowerOff(host); err != nil {
			return errors.Wrap(err, "ssh power off")
		}
	}

	if err := host.Stop(); err != nil {
		glog.Infof("host.Stop failed: %v", err)
		alreadyInStateError, ok := err.(mcnerror.ErrHostAlreadyInState)
		if ok && alreadyInStateError.State == state.Stopped {
			return nil
		}
		return &retry.RetriableError{Err: errors.Wrapf(err, "Stop: %s", machineName)}
	}
	return nil
}

// deleteOrphanedKIC attempts to delete an orphaned docker instance
func deleteOrphanedKIC(name string) {
	cmd := exec.Command(oci.Docker, "rm", "-f", "-v", name)
	err := cmd.Run()
	if err == nil {
		glog.Infof("Found stale kic container and successfully cleaned it up!")
	}
}

// DeleteHost deletes the host VM.
func DeleteHost(api libmachine.API, machineName string) error {
	host, err := api.Load(machineName)
	if err != nil && host == nil {
		deleteOrphanedKIC(machineName)
		// keep going even if minikube  does not know about the host
	}

	// Get the status of the host. Ensure that it exists before proceeding ahead.
	status, err := GetHostStatus(api, machineName)
	if err != nil {
		// Warn, but proceed
		out.WarningT("Unable to get the status of the {{.name}} cluster.", out.V{"name": machineName})
	}

	if status == state.None.String() {
		return mcnerror.ErrHostDoesNotExist{Name: machineName}
	}

	// This is slow if SSH is not responding, but HyperV hangs otherwise, See issue #2914
	if host.Driver.DriverName() == driver.HyperV {
		if err := trySSHPowerOff(host); err != nil {
			glog.Infof("Unable to power off minikube because the host was not found.")
		}
		out.T(out.DeletingHost, "Successfully powered off Hyper-V. minikube driver -- {{.driver}}", out.V{"driver": host.Driver.DriverName()})
	}

	out.T(out.DeletingHost, `Deleting "{{.profile_name}}" in {{.driver_name}} ...`, out.V{"profile_name": machineName, "driver_name": host.DriverName})
	if err := host.Driver.Remove(); err != nil {
		return errors.Wrap(err, "host remove")
	}
	if err := api.Remove(machineName); err != nil {
		return errors.Wrap(err, "api remove")
	}
	return nil
}

// GetHostStatus gets the status of the host VM.
func GetHostStatus(api libmachine.API, machineName string) (string, error) {
	exists, err := api.Exists(machineName)
	if err != nil {
		return "", errors.Wrapf(err, "%s exists", machineName)
	}
	if !exists {
		return state.None.String(), nil
	}

	host, err := api.Load(machineName)
	if err != nil {
		return "", errors.Wrapf(err, "load")
	}

	s, err := host.Driver.GetState()
	if err != nil {
		return "", errors.Wrap(err, "state")
	}
	return s.String(), nil
}

// GetHostDriverIP gets the ip address of the current minikube cluster
func GetHostDriverIP(api libmachine.API, machineName string) (net.IP, error) {
	host, err := CheckIfHostExistsAndLoad(api, machineName)
	if err != nil {
		return nil, err
	}

	ipStr, err := host.Driver.GetIP()
	if err != nil {
		return nil, errors.Wrap(err, "getting IP")
	}
	if driver.IsKIC(host.DriverName) {
		ipStr = kic.DefaultBindIPV4
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return nil, fmt.Errorf("parsing IP: %s", ipStr)
	}
	return ip, nil
}

func engineOptions(cfg config.MachineConfig) *engine.Options {
	o := engine.Options{
		Env:              cfg.DockerEnv,
		InsecureRegistry: append([]string{constants.DefaultServiceCIDR}, cfg.InsecureRegistry...),
		RegistryMirror:   cfg.RegistryMirror,
		ArbitraryFlags:   cfg.DockerOpt,
		InstallURL:       drivers.DefaultEngineInstallURL,
	}
	return &o
}

type hostInfo struct {
	Memory   int
	CPUs     int
	DiskSize int
}

func megs(bytes uint64) int {
	return int(bytes / 1024 / 1024)
}

func getHostInfo() (*hostInfo, error) {
	i, err := cpu.Info()
	if err != nil {
		glog.Warningf("Unable to get CPU info: %v", err)
		return nil, err
	}
	v, err := mem.VirtualMemory()
	if err != nil {
		glog.Warningf("Unable to get mem info: %v", err)
		return nil, err
	}
	d, err := disk.Usage("/")
	if err != nil {
		glog.Warningf("Unable to get disk info: %v", err)
		return nil, err
	}

	var info hostInfo
	info.CPUs = len(i)
	info.Memory = megs(v.Total)
	info.DiskSize = megs(d.Total)
	return &info, nil
}

// showLocalOsRelease shows systemd information about the current linux distribution, on the local host
func showLocalOsRelease() {
	osReleaseOut, err := ioutil.ReadFile("/etc/os-release")
	if err != nil {
		glog.Errorf("ReadFile: %v", err)
		return
	}

	osReleaseInfo, err := provision.NewOsRelease(osReleaseOut)
	if err != nil {
		glog.Errorf("NewOsRelease: %v", err)
		return
	}

	out.T(out.Provisioner, "OS release is {{.pretty_name}}", out.V{"pretty_name": osReleaseInfo.PrettyName})
}

// showRemoteOsRelease shows systemd information about the current linux distribution, on the remote VM
func showRemoteOsRelease(driver drivers.Driver) {
	provisioner, err := provision.DetectProvisioner(driver)
	if err != nil {
		glog.Errorf("DetectProvisioner: %v", err)
		return
	}

	osReleaseInfo, err := provisioner.GetOsReleaseInfo()
	if err != nil {
		glog.Errorf("GetOsReleaseInfo: %v", err)
		return
	}

	glog.Infof("Provisioned with %s", osReleaseInfo.PrettyName)
}

// showHostInfo shows host information
func showHostInfo(cfg config.MachineConfig) {
	if driver.BareMetal(cfg.VMDriver) {
		info, err := getHostInfo()
		if err == nil {
			out.T(out.StartingNone, "Running on localhost (CPUs={{.number_of_cpus}}, Memory={{.memory_size}}MB, Disk={{.disk_size}}MB) ...", out.V{"number_of_cpus": info.CPUs, "memory_size": info.Memory, "disk_size": info.DiskSize})
		}
	} else if driver.IsKIC(cfg.VMDriver) {
		info, err := getHostInfo() // TODO medyagh: get docker-machine info for non linux
		if err == nil {
			out.T(out.StartingVM, "Creating Kubernetes in {{.driver_name}} container with (CPUs={{.number_of_cpus}}), Memory={{.memory_size}}MB ({{.host_memory_size}}MB available) ...", out.V{"driver_name": cfg.VMDriver, "number_of_cpus": cfg.CPUs, "number_of_host_cpus": info.CPUs, "memory_size": cfg.Memory, "host_memory_size": info.Memory})
		}
	} else {
		out.T(out.StartingVM, "Creating {{.driver_name}} VM (CPUs={{.number_of_cpus}}, Memory={{.memory_size}}MB, Disk={{.disk_size}}MB) ...", out.V{"driver_name": cfg.VMDriver, "number_of_cpus": cfg.CPUs, "memory_size": cfg.Memory, "disk_size": cfg.DiskSize})
	}
}

func createHost(api libmachine.API, cfg config.MachineConfig) (*host.Host, error) {
	if cfg.VMDriver == driver.VMwareFusion && viper.GetBool(config.ShowDriverDeprecationNotification) {
		out.WarningT(`The vmwarefusion driver is deprecated and support for it will be removed in a future release.
			Please consider switching to the new vmware unified driver, which is intended to replace the vmwarefusion driver.
			See https://minikube.sigs.k8s.io/docs/reference/drivers/vmware/ for more information.
			To disable this message, run [minikube config set ShowDriverDeprecationNotification false]`)
	}
	showHostInfo(cfg)
	def := registry.Driver(cfg.VMDriver)
	if def.Empty() {
		return nil, fmt.Errorf("unsupported/missing driver: %s", cfg.VMDriver)
	}
	dd := def.Config(cfg)
	data, err := json.Marshal(dd)
	if err != nil {
		return nil, errors.Wrap(err, "marshal")
	}

	h, err := api.NewHost(cfg.VMDriver, data)
	if err != nil {
		return nil, errors.Wrap(err, "new host")
	}

	h.HostOptions.AuthOptions.CertDir = localpath.MiniPath()
	h.HostOptions.AuthOptions.StorePath = localpath.MiniPath()
	h.HostOptions.EngineOptions = engineOptions(cfg)

	if err := api.Create(h); err != nil {
		// Wait for all the logs to reach the client
		time.Sleep(2 * time.Second)
		return nil, errors.Wrap(err, "create")
	}

	if err := createRequiredDirectories(h); err != nil {
		errors.Wrap(err, "required directories")
	}

	if driver.BareMetal(cfg.VMDriver) {
		showLocalOsRelease()
	} else if !driver.BareMetal(cfg.VMDriver) && !driver.IsKIC(cfg.VMDriver) {
		showRemoteOsRelease(h.Driver)
		// Ensure that even new VM's have proper time synchronization up front
		// It's 2019, and I can't believe I am still dealing with time desync as a problem.
		if err := ensureSyncedGuestClock(h); err != nil {
			return h, err
		}
	} // TODO:medyagh add show-os release for kic

	if err := api.Save(h); err != nil {
		return nil, errors.Wrap(err, "save")
	}
	return h, nil
}

// GetHostDockerEnv gets the necessary docker env variables to allow the use of docker through minikube's vm
func GetHostDockerEnv(api libmachine.API) (map[string]string, error) {
	pName := viper.GetString(config.MachineProfile)
	host, err := CheckIfHostExistsAndLoad(api, pName)
	if err != nil {
		return nil, errors.Wrap(err, "Error checking that api exists and loading it")
	}

	ip := kic.DefaultBindIPV4
	if !driver.IsKIC(host.Driver.DriverName()) { // kic externally accessible ip is different that node ip
		ip, err = host.Driver.GetIP()
		if err != nil {
			return nil, errors.Wrap(err, "Error getting ip from host")
		}

	}

	tcpPrefix := "tcp://"
	port := constants.DockerDaemonPort
	if driver.IsKIC(host.Driver.DriverName()) { // for kic we need to find out what port docker allocated during creation
		port, err = oci.HostPortBinding(host.Driver.DriverName(), pName, constants.DockerDaemonPort)
		if err != nil {
			return nil, errors.Wrapf(err, "get hostbind port for %d", constants.DockerDaemonPort)
		}
	}

	envMap := map[string]string{
		"DOCKER_TLS_VERIFY": "1",
		"DOCKER_HOST":       tcpPrefix + net.JoinHostPort(ip, fmt.Sprint(port)),
		"DOCKER_CERT_PATH":  localpath.MakeMiniPath("certs"),
	}
	return envMap, nil
}

// GetVMHostIP gets the ip address to be used for mapping host -> VM and VM -> host
func GetVMHostIP(host *host.Host) (net.IP, error) {
	switch host.DriverName {
	case driver.KVM2:
		return net.ParseIP("192.168.39.1"), nil
	case driver.HyperV:
		re := regexp.MustCompile(`"VSwitch": "(.*?)",`)
		// TODO(aprindle) Change this to deserialize the driver instead
		hypervVirtualSwitch := re.FindStringSubmatch(string(host.RawDriver))[1]
		ip, err := getIPForInterface(fmt.Sprintf("vEthernet (%s)", hypervVirtualSwitch))
		if err != nil {
			return []byte{}, errors.Wrap(err, fmt.Sprintf("ip for interface (%s)", hypervVirtualSwitch))
		}
		return ip, nil
	case driver.VirtualBox:
		out, err := exec.Command(driver.VBoxManagePath(), "showvminfo", host.Name, "--machinereadable").Output()
		if err != nil {
			return []byte{}, errors.Wrap(err, "vboxmanage")
		}
		re := regexp.MustCompile(`hostonlyadapter2="(.*?)"`)
		iface := re.FindStringSubmatch(string(out))[1]
		ip, err := getIPForInterface(iface)
		if err != nil {
			return []byte{}, errors.Wrap(err, "Error getting VM/Host IP address")
		}
		return ip, nil
	case driver.HyperKit:
		return net.ParseIP("192.168.64.1"), nil
	case driver.VMware:
		vmIPString, err := host.Driver.GetIP()
		if err != nil {
			return []byte{}, errors.Wrap(err, "Error getting VM IP address")
		}
		vmIP := net.ParseIP(vmIPString).To4()
		if vmIP == nil {
			return []byte{}, errors.Wrap(err, "Error converting VM IP address to IPv4 address")
		}
		return net.IPv4(vmIP[0], vmIP[1], vmIP[2], byte(1)), nil
	default:
		return []byte{}, errors.New("Error, attempted to get host ip address for unsupported driver")
	}
}

// Based on code from http://stackoverflow.com/questions/23529663/how-to-get-all-addresses-and-masks-from-local-interfaces-in-go
func getIPForInterface(name string) (net.IP, error) {
	i, _ := net.InterfaceByName(name)
	addrs, _ := i.Addrs()
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			if ip := ipnet.IP.To4(); ip != nil {
				return ip, nil
			}
		}
	}
	return nil, errors.Errorf("Error finding IPV4 address for %s", name)
}

// CheckIfHostExistsAndLoad checks if a host exists, and loads it if it does
func CheckIfHostExistsAndLoad(api libmachine.API, machineName string) (*host.Host, error) {
	glog.Infof("Checking if %q exists ...", machineName)
	exists, err := api.Exists(machineName)
	if err != nil {
		return nil, errors.Wrapf(err, "Error checking that machine exists: %s", machineName)
	}
	if !exists {
		return nil, errors.Errorf("machine %q does not exist", machineName)
	}

	host, err := api.Load(machineName)
	if err != nil {
		return nil, errors.Wrapf(err, "loading machine %q", machineName)
	}
	return host, nil
}

// CreateSSHShell creates a new SSH shell / client
func CreateSSHShell(api libmachine.API, args []string) error {
	machineName := viper.GetString(config.MachineProfile)
	host, err := CheckIfHostExistsAndLoad(api, machineName)
	if err != nil {
		return errors.Wrap(err, "host exists and load")
	}

	currentState, err := host.Driver.GetState()
	if err != nil {
		return errors.Wrap(err, "state")
	}

	if currentState != state.Running {
		return errors.Errorf("%q is not running", machineName)
	}

	client, err := host.CreateSSHClient()
	if err != nil {
		return errors.Wrap(err, "Creating ssh client")
	}
	return client.Shell(args...)
}

// IsHostRunning asserts that this profile's primary host is in state "Running"
func IsHostRunning(api libmachine.API, name string) bool {
	s, err := GetHostStatus(api, name)
	if err != nil {
		glog.Warningf("host status for %q returned error: %v", name, err)
		return false
	}
	if s != state.Running.String() {
		glog.Warningf("%q host status: %s", name, s)
		return false
	}
	return true
}

// createRequiredDirectories creates directories expected by minikube to exist
func createRequiredDirectories(h *host.Host) error {
	if h.DriverName == driver.Mock {
		glog.Infof("skipping createRequiredDirectories")
		return nil
	}
	glog.Infof("creating required directories: %v", requiredDirectories)
	r, err := commandRunner(h)
	if err != nil {
		return errors.Wrap(err, "command runner")
	}

	args := append([]string{"mkdir", "-p"}, requiredDirectories...)
	if _, err := r.RunCmd(exec.Command("sudo", args...)); err != nil {
		return errors.Wrapf(err, "sudo mkdir (%s)", h.DriverName)
	}
	return nil
}

// commandRunner returns best available command runner for this host
func commandRunner(h *host.Host) (command.Runner, error) {
	if h.DriverName == driver.Mock {
		glog.Errorf("commandRunner: returning unconfigured FakeCommandRunner, commands will fail!")
		return &command.FakeCommandRunner{}, nil
	}
	if driver.BareMetal(h.Driver.DriverName()) {
		return &command.ExecRunner{}, nil
	}
	if h.Driver.DriverName() == driver.Docker {
		return command.NewKICRunner(h.Name, "docker"), nil
	}
	client, err := sshutil.NewSSHClient(h.Driver)
	if err != nil {
		return nil, errors.Wrap(err, "getting ssh client for bootstrapper")
	}
	return command.NewSSHRunner(client), nil
}
