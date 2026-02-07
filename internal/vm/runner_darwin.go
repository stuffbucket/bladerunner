//go:build darwin

package vm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Code-Hex/vz/v3"
	"github.com/stuffbucket/bladerunner/internal/config"
	incusctl "github.com/stuffbucket/bladerunner/internal/incus"
	"github.com/stuffbucket/bladerunner/internal/logging"
	"github.com/stuffbucket/bladerunner/internal/provision"
	"github.com/stuffbucket/bladerunner/internal/report"
	"github.com/stuffbucket/bladerunner/internal/ssh"
)

type Runner struct {
	cfg *config.Config

	vm            *vz.VirtualMachine
	metadata      *runtimeMetadata
	clientCrt     []byte
	clientKey     []byte
	baseImagePath string

	forwarders []*portForwarder
	stopOnce   sync.Once
	stopErr    error
}

// StartVMResult contains the initial state after VM starts running.
type StartVMResult struct {
	Endpoint string
}

func NewRunner(cfg *config.Config) (*Runner, error) {
	if cfg == nil {
		return nil, errors.New("config is nil")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Runner{cfg: cfg}, nil
}

// StartVM provisions and starts the VM, returning as soon as it's running.
// Call WaitForIncus() separately to wait for cloud-init and Incus API readiness.
func (r *Runner) StartVM(ctx context.Context) (*StartVMResult, error) {
	log := logging.L()
	log.Info("starting VM provisioning", "name", r.cfg.Name, "vm_dir", r.cfg.VMDir, "cpus", r.cfg.CPUs, "memory_gib", r.cfg.MemoryGiB)

	if err := ensureVMDir(r.cfg); err != nil {
		return nil, err
	}

	log.Info("ensuring client TLS credentials")
	certPEM, keyPEM, err := incusctl.EnsureClientCertificate(r.cfg.ClientCertPath, r.cfg.ClientKeyPath)
	if err != nil {
		return nil, err
	}
	r.clientCrt = certPEM
	r.clientKey = keyPEM

	log.Info("building cloud-init payload")
	userData, metaData := provision.BuildCloudInit(r.cfg, string(certPEM))
	if err := provision.WriteSeedFiles(r.cfg, userData, metaData); err != nil {
		return nil, err
	}
	if err := provision.BuildCloudInitISO(ctx, r.cfg); err != nil {
		return nil, err
	}

	log.Info("resolving base image and main disk")
	baseImagePath, err := ensureBaseImage(ctx, r.cfg)
	if err != nil {
		return nil, err
	}
	r.baseImagePath = baseImagePath
	if err := ensureMainDisk(r.cfg, baseImagePath); err != nil {
		return nil, err
	}

	md, err := loadOrCreateMetadata(r.cfg)
	if err != nil {
		return nil, err
	}
	r.metadata = md

	log.Info("constructing virtual machine configuration")
	vmCfg, err := r.newVMConfiguration()
	if err != nil {
		return nil, err
	}

	log.Info("creating virtual machine instance")
	vm, err := vz.NewVirtualMachine(vmCfg)
	if err != nil {
		return nil, fmt.Errorf("create vm: %w", err)
	}
	r.vm = vm

	log.Info("starting virtual machine")
	if err := vm.Start(); err != nil {
		return nil, fmt.Errorf("start vm: %w", err)
	}

	vmWait := logging.NewTimedProgress("Waiting for VM to reach running state", 2*time.Minute)
	if err := r.waitForRunning(ctx, 2*time.Minute, func(st vz.VirtualMachineState) {
		msg := fmt.Sprintf("state=%s", st.String())
		vmWait.SetStatus(msg)
		log.Info("vm state changed", "state", st.String())
	}); err != nil {
		vmWait.Fail(err)
		return nil, err
	}
	vmWait.SetStatus("running")
	vmWait.Finish()

	log.Info("starting localhost forwarders")
	if err := r.startForwarders(); err != nil {
		return nil, err
	}

	endpoint := fmt.Sprintf("https://127.0.0.1:%d", r.cfg.LocalAPIPort)
	return &StartVMResult{Endpoint: endpoint}, nil
}

// WaitForIncus waits for the Incus API to become ready and returns a startup report.
func (r *Runner) WaitForIncus(ctx context.Context) (*report.StartupReport, error) {
	log := logging.L()
	endpoint := fmt.Sprintf("https://127.0.0.1:%d", r.cfg.LocalAPIPort)

	incusCtx, cancel := context.WithTimeout(ctx, r.cfg.WaitForIncus)
	defer cancel()

	incusWait := logging.NewTimedProgress("Waiting for Incus API readiness", r.cfg.WaitForIncus)
	serverInfo, err := incusctl.WaitForServer(incusCtx, endpoint, r.clientCrt, r.clientKey, 4*time.Second, func(p incusctl.WaitProgress) {
		incusWait.SetStatus(fmt.Sprintf("attempt=%d %s", p.Attempt, summarizeErr(p.LastError)))
	})
	if err != nil {
		incusWait.Fail(err)
		log.Warn("incus api was not ready before timeout; continuing with partial report", "endpoint", endpoint, "err", err)
		serverInfo = nil
	} else {
		incusWait.SetStatus("ready")
		incusWait.Finish()
	}

	log.Info("assembling startup report")
	reportData := r.makeReport(r.baseImagePath, endpoint, serverInfo)
	if err := report.SaveJSON(r.cfg.ReportPath, reportData); err != nil {
		return nil, err
	}
	log.Info("startup report saved", "path", r.cfg.ReportPath)

	return reportData, nil
}

// Start provisions, starts, and waits for Incus. Convenience wrapper for StartVM + WaitForIncus.
func (r *Runner) Start(ctx context.Context) (*report.StartupReport, error) {
	if _, err := r.StartVM(ctx); err != nil {
		return nil, err
	}
	return r.WaitForIncus(ctx)
}

func (r *Runner) StartGUI() error {
	if r.vm == nil {
		return errors.New("vm is not running")
	}
	logging.L().Info("starting GUI console")

	return r.vm.StartGraphicApplication(1920, 1200, vz.WithWindowTitle("Bladerunner Incus VM"), vz.WithController(true))
}

func (r *Runner) Wait(ctx context.Context) error {
	if r.vm == nil {
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			logging.L().Warn("wait context canceled", "err", ctx.Err())
			return ctx.Err()
		case st := <-r.vm.StateChangedNotify():
			logging.L().Info("vm lifecycle event", "state", st.String())
			switch st {
			case vz.VirtualMachineStateError:
				return fmt.Errorf("vm entered error state")
			case vz.VirtualMachineStateStopped:
				return nil
			}
		}
	}
}

func (r *Runner) Stop() error {
	r.stopOnce.Do(func() {
		log := logging.L()
		log.Info("stopping vm and forwarders")
		for _, f := range r.forwarders {
			if err := f.Close(); err != nil && r.stopErr == nil {
				r.stopErr = err
			}
		}

		if r.vm == nil {
			return
		}

		for i := 0; i < 3 && r.vm.CanRequestStop(); i++ {
			ok, err := r.vm.RequestStop()
			log.Info("sent stop request", "attempt", i+1, "accepted", ok, "err", err)
			if err != nil && r.stopErr == nil {
				r.stopErr = err
			}
			time.Sleep(2 * time.Second)
		}

		if r.vm.CanStop() {
			if err := r.vm.Stop(); err != nil {
				log.Warn("forced stop failed", "err", err)
				if r.stopErr == nil {
					r.stopErr = err
				}
			}
		}
	})

	return r.stopErr
}

func (r *Runner) waitForRunning(ctx context.Context, timeout time.Duration, onState func(vz.VirtualMachineState)) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if onState != nil {
		onState(r.vm.State())
	}

	for {
		if r.vm.State() == vz.VirtualMachineStateRunning {
			return nil
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("vm did not become running: %w", waitCtx.Err())
		case st := <-r.vm.StateChangedNotify():
			if onState != nil {
				onState(st)
			}
			switch st {
			case vz.VirtualMachineStateRunning:
				return nil
			case vz.VirtualMachineStateError:
				return errors.New("vm entered error state during startup")
			case vz.VirtualMachineStateStopped:
				return errors.New("vm stopped during startup")
			}
		}
	}
}

func (r *Runner) startForwarders() error {
	socketDevices := r.vm.SocketDevices()
	if len(socketDevices) == 0 {
		return errors.New("vm has no virtio socket device configured")
	}
	device := socketDevices[0]

	dial := func(port uint32) (net.Conn, error) {
		return device.Connect(port)
	}

	sshForward := newPortForwarder(
		"ssh",
		fmt.Sprintf("127.0.0.1:%d", r.cfg.LocalSSHPort),
		r.cfg.VsockSSHPort,
		dial,
	)
	if err := sshForward.Start(); err != nil {
		return fmt.Errorf("start ssh forwarder: %w", err)
	}

	apiForward := newPortForwarder(
		"incus-api",
		fmt.Sprintf("127.0.0.1:%d", r.cfg.LocalAPIPort),
		r.cfg.VsockAPIPort,
		dial,
	)
	if err := apiForward.Start(); err != nil {
		_ = sshForward.Close()
		return fmt.Errorf("start api forwarder: %w", err)
	}

	r.forwarders = []*portForwarder{sshForward, apiForward}
	logging.L().Info("forwarders active", "ssh", fmt.Sprintf("127.0.0.1:%d", r.cfg.LocalSSHPort), "api", fmt.Sprintf("127.0.0.1:%d", r.cfg.LocalAPIPort))
	return nil
}

func (r *Runner) newVMConfiguration() (*vz.VirtualMachineConfiguration, error) {
	bootLoader, err := r.newEFIBootLoader()
	if err != nil {
		return nil, err
	}

	cpu := clampCPU(r.cfg.CPUs)
	mem := clampMemory(r.cfg.MemoryGiB * 1024 * 1024 * 1024)

	cfg, err := vz.NewVirtualMachineConfiguration(bootLoader, cpu, mem)
	if err != nil {
		return nil, fmt.Errorf("new virtual machine configuration: %w", err)
	}

	if err := r.configurePlatform(cfg); err != nil {
		return nil, err
	}
	if err := r.configureStorage(cfg); err != nil {
		return nil, err
	}
	if err := r.configureNetwork(cfg); err != nil {
		return nil, err
	}
	if r.cfg.GUI {
		if err := r.configureGraphics(cfg); err != nil {
			return nil, err
		}
	}
	if err := r.configureSerial(cfg); err != nil {
		return nil, err
	}
	if err := r.configureMisc(cfg); err != nil {
		return nil, err
	}

	ok, err := cfg.Validate()
	if err != nil {
		return nil, fmt.Errorf("validate vm configuration: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("vm configuration is invalid")
	}

	return cfg, nil
}

func (r *Runner) configurePlatform(cfg *vz.VirtualMachineConfiguration) error {
	identifier, err := r.loadOrCreateMachineIdentifier()
	if err != nil {
		return err
	}

	platformConfig, err := vz.NewGenericPlatformConfiguration(vz.WithGenericMachineIdentifier(identifier))
	if err != nil {
		return fmt.Errorf("create generic platform configuration: %w", err)
	}

	cfg.SetPlatformVirtualMachineConfiguration(platformConfig)
	return nil
}

func (r *Runner) configureStorage(cfg *vz.VirtualMachineConfiguration) error {
	mainDiskAttach, err := vz.NewDiskImageStorageDeviceAttachment(r.cfg.DiskPath, false)
	if err != nil {
		return fmt.Errorf("create main disk attachment: %w", err)
	}
	mainDisk, err := vz.NewVirtioBlockDeviceConfiguration(mainDiskAttach)
	if err != nil {
		return fmt.Errorf("create main block config: %w", err)
	}

	cloudInitAttach, err := vz.NewDiskImageStorageDeviceAttachment(r.cfg.CloudInitISO, true)
	if err != nil {
		return fmt.Errorf("create cloud-init disk attachment: %w", err)
	}
	cloudInitDisk, err := vz.NewVirtioBlockDeviceConfiguration(cloudInitAttach)
	if err != nil {
		return fmt.Errorf("create cloud-init block config: %w", err)
	}

	cfg.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{mainDisk, cloudInitDisk})
	return nil
}

func (r *Runner) configureNetwork(cfg *vz.VirtualMachineConfiguration) error {
	attachment, err := r.newNetworkAttachment()
	if err != nil {
		return err
	}

	netCfg, err := vz.NewVirtioNetworkDeviceConfiguration(attachment)
	if err != nil {
		return fmt.Errorf("create virtio net config: %w", err)
	}

	hw, err := net.ParseMAC(r.metadata.MACAddress)
	if err != nil {
		return fmt.Errorf("parse persisted mac address %q: %w", r.metadata.MACAddress, err)
	}

	mac, err := vz.NewMACAddress(hw)
	if err != nil {
		return fmt.Errorf("create mac address: %w", err)
	}
	netCfg.SetMACAddress(mac)

	cfg.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{netCfg})
	return nil
}

func (r *Runner) newNetworkAttachment() (vz.NetworkDeviceAttachment, error) {
	if r.cfg.NetworkMode == config.NetworkModeBridged {
		for _, iface := range vz.NetworkInterfaces() {
			if iface.Identifier() == r.cfg.BridgeInterface || strings.EqualFold(iface.LocalizedDisplayName(), r.cfg.BridgeInterface) {
				bridge, err := vz.NewBridgedNetworkDeviceAttachment(iface)
				if err != nil {
					return nil, fmt.Errorf("create bridged attachment for %s: %w", iface.Identifier(), err)
				}
				return bridge, nil
			}
		}
		return nil, fmt.Errorf("bridged interface %s was not found", r.cfg.BridgeInterface)
	}

	nat, err := vz.NewNATNetworkDeviceAttachment()
	if err != nil {
		return nil, fmt.Errorf("create nat attachment: %w", err)
	}
	return nat, nil
}

func (r *Runner) configureGraphics(cfg *vz.VirtualMachineConfiguration) error {
	graphics, err := vz.NewVirtioGraphicsDeviceConfiguration()
	if err != nil {
		return fmt.Errorf("create virtio graphics config: %w", err)
	}

	scanout, err := vz.NewVirtioGraphicsScanoutConfiguration(1920, 1200)
	if err != nil {
		return fmt.Errorf("create graphics scanout: %w", err)
	}
	graphics.SetScanouts(scanout)

	pointing, err := vz.NewUSBScreenCoordinatePointingDeviceConfiguration()
	if err != nil {
		return fmt.Errorf("create pointing device config: %w", err)
	}

	keyboard, err := vz.NewUSBKeyboardConfiguration()
	if err != nil {
		return fmt.Errorf("create keyboard config: %w", err)
	}

	cfg.SetGraphicsDevicesVirtualMachineConfiguration([]vz.GraphicsDeviceConfiguration{graphics})
	cfg.SetPointingDevicesVirtualMachineConfiguration([]vz.PointingDeviceConfiguration{pointing})
	cfg.SetKeyboardsVirtualMachineConfiguration([]vz.KeyboardConfiguration{keyboard})

	return nil
}

func (r *Runner) configureSerial(cfg *vz.VirtualMachineConfiguration) error {
	if err := os.MkdirAll(filepath.Dir(r.cfg.ConsoleLogPath), 0o755); err != nil {
		return fmt.Errorf("create console log parent: %w", err)
	}
	serialAttachment, err := vz.NewFileSerialPortAttachment(r.cfg.ConsoleLogPath, true)
	if err != nil {
		return fmt.Errorf("create serial file attachment: %w", err)
	}
	serialConfig, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(serialAttachment)
	if err != nil {
		return fmt.Errorf("create serial port config: %w", err)
	}
	cfg.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{serialConfig})
	return nil
}

func (r *Runner) configureMisc(cfg *vz.VirtualMachineConfiguration) error {
	entropy, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err == nil {
		cfg.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropy})
	}

	balloon, err := vz.NewVirtioTraditionalMemoryBalloonDeviceConfiguration()
	if err == nil {
		cfg.SetMemoryBalloonDevicesVirtualMachineConfiguration([]vz.MemoryBalloonDeviceConfiguration{balloon})
	}

	vsockConfig, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return fmt.Errorf("create virtio socket config: %w", err)
	}
	cfg.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{vsockConfig})

	return nil
}

func (r *Runner) newEFIBootLoader() (*vz.EFIBootLoader, error) {
	varStore, err := loadOrCreateEFIVariableStore(r.cfg.EFIVarsPath)
	if err != nil {
		return nil, err
	}

	bootLoader, err := vz.NewEFIBootLoader(vz.WithEFIVariableStore(varStore))
	if err != nil {
		return nil, fmt.Errorf("create efi bootloader: %w", err)
	}
	return bootLoader, nil
}

func loadOrCreateEFIVariableStore(path string) (*vz.EFIVariableStore, error) {
	if fileExists(path) {
		store, err := vz.NewEFIVariableStore(path)
		if err != nil {
			return nil, fmt.Errorf("load efi variable store: %w", err)
		}
		return store, nil
	}

	store, err := vz.NewEFIVariableStore(path, vz.WithCreatingEFIVariableStore())
	if err != nil {
		return nil, fmt.Errorf("create efi variable store: %w", err)
	}
	return store, nil
}

func (r *Runner) loadOrCreateMachineIdentifier() (*vz.GenericMachineIdentifier, error) {
	if fileExists(r.cfg.MachineIDPath) {
		identifier, err := vz.NewGenericMachineIdentifierWithDataPath(r.cfg.MachineIDPath)
		if err != nil {
			return nil, fmt.Errorf("load machine identifier: %w", err)
		}
		return identifier, nil
	}

	identifier, err := vz.NewGenericMachineIdentifier()
	if err != nil {
		return nil, fmt.Errorf("create machine identifier: %w", err)
	}

	if err := os.WriteFile(r.cfg.MachineIDPath, identifier.DataRepresentation(), 0o644); err != nil {
		return nil, fmt.Errorf("persist machine identifier: %w", err)
	}

	return identifier, nil
}

func clampCPU(requested uint) uint {
	cpu := requested
	if cpu < 1 {
		cpu = 1
	}
	maxCPU := vz.VirtualMachineConfigurationMaximumAllowedCPUCount()
	minCPU := vz.VirtualMachineConfigurationMinimumAllowedCPUCount()
	if cpu > maxCPU {
		cpu = maxCPU
	}
	if cpu < minCPU {
		cpu = minCPU
	}
	return cpu
}

func clampMemory(bytes uint64) uint64 {
	mem := bytes
	maxMem := vz.VirtualMachineConfigurationMaximumAllowedMemorySize()
	minMem := vz.VirtualMachineConfigurationMinimumAllowedMemorySize()
	if mem > maxMem {
		mem = maxMem
	}
	if mem < minMem {
		mem = minMem
	}

	// Must be in MiB increments.
	const mib = 1024 * 1024
	mem = (mem / mib) * mib
	if mem < minMem {
		mem = minMem
	}
	return mem
}

func (r *Runner) makeReport(baseImagePath, endpoint string, server *incusctl.ServerInfo) *report.StartupReport {
	sshEndpoint := fmt.Sprintf("127.0.0.1:%d", r.cfg.LocalSSHPort)
	apiEndpoint := fmt.Sprintf("127.0.0.1:%d", r.cfg.LocalAPIPort)

	// Write SSH config file for easy VM access
	var sshCommand string
	var sshConfigPath string
	if r.cfg.SSHPrivateKeyPath != "" {
		configPath, err := ssh.WriteSSHConfig(r.cfg.Name, r.cfg.LocalSSHPort, r.cfg.SSHUser, r.cfg.SSHPrivateKeyPath)
		if err != nil {
			logging.L().Warn("failed to write SSH config", "err", err)
			sshCommand = fmt.Sprintf("ssh -p %d -i %s %s@127.0.0.1", r.cfg.LocalSSHPort, r.cfg.SSHPrivateKeyPath, r.cfg.SSHUser)
		} else {
			sshConfigPath = configPath
			r.cfg.SSHConfigPath = configPath
			sshCommand = ssh.Command(configPath, r.cfg.Name)
		}
	} else {
		sshCommand = fmt.Sprintf("ssh -p %d %s@127.0.0.1", r.cfg.LocalSSHPort, r.cfg.SSHUser)
	}

	data := &report.StartupReport{
		GeneratedAt: time.Now().UTC(),
		Host: report.HostInfo{
			OS:           runtime.GOOS,
			Arch:         runtime.GOARCH,
			CPUCount:     runtime.NumCPU(),
			RequestedCPU: r.cfg.CPUs,
		},
		VM: report.VMInfo{
			Name:          r.cfg.Name,
			Hostname:      r.cfg.Hostname,
			Directory:     r.cfg.VMDir,
			DiskPath:      r.cfg.DiskPath,
			DiskSizeGiB:   r.cfg.DiskSizeGiB,
			MemoryGiB:     r.cfg.MemoryGiB,
			GuestArch:     runtime.GOARCH,
			GUIEnabled:    r.cfg.GUI,
			ConsoleLog:    r.cfg.ConsoleLogPath,
			CloudInitISO:  r.cfg.CloudInitISO,
			BaseImagePath: baseImagePath,
			BaseImageURL:  r.cfg.BaseImageURL,
		},
		Network: report.NetInfo{
			Mode:             r.cfg.NetworkMode,
			BridgeInterface:  bridgeField(r.cfg),
			MACAddress:       r.metadata.MACAddress,
			LocalSSHEndpoint: sshEndpoint,
			LocalAPIEndpoint: apiEndpoint,
			DashboardURL:     fmt.Sprintf("https://%s%s", apiEndpoint, r.cfg.DashboardPath),
		},
		Access: report.Access{
			SSHCommand:          sshCommand,
			SSHConfigPath:       sshConfigPath,
			SSHKeyPath:          r.cfg.SSHPrivateKeyPath,
			RESTExample:         fmt.Sprintf("curl --cert %s --key %s -k %s/1.0", r.cfg.ClientCertPath, r.cfg.ClientKeyPath, endpoint),
			GoClientExamplePath: filepath.Join(r.cfg.VMDir, "incus-client-example.go"),
			ClientCertPath:      r.cfg.ClientCertPath,
			ClientKeyPath:       r.cfg.ClientKeyPath,
			LogPath:             r.cfg.LogPath,
		},
	}

	if server != nil {
		data.Incus = report.IncusInfo{
			ServerVersion: server.ServerVersion,
			APIVersion:    server.APIVersion,
			Auth:          server.Auth,
			ServerName:    server.ServerName,
			Addresses:     append([]string{}, server.Addresses...),
			APIExtensions: server.APIExtensions,
		}
	}

	_ = os.WriteFile(data.Access.GoClientExamplePath, []byte(r.goClientExample()), 0o644)
	return data
}

func (r *Runner) goClientExample() string {
	return fmt.Sprintf(`package main

import (
	"fmt"
	"os"

	incus "github.com/lxc/incus/v6/client"
)

func main() {
	cert, err := os.ReadFile(%q)
	if err != nil {
		panic(err)
	}
	key, err := os.ReadFile(%q)
	if err != nil {
		panic(err)
	}

	client, err := incus.ConnectIncus("https://127.0.0.1:%d", &incus.ConnectionArgs{
		TLSClientCert: string(cert),
		TLSClientKey:  string(key),
		InsecureSkipVerify: true,
	})
	if err != nil {
		panic(err)
	}

	server, _, err := client.GetServer()
	if err != nil {
		panic(err)
	}

	fmt.Println("Connected to", server.Environment.Server)
}
`, r.cfg.ClientCertPath, r.cfg.ClientKeyPath, r.cfg.LocalAPIPort)
}

func bridgeField(cfg *config.Config) string {
	if cfg.NetworkMode == config.NetworkModeBridged {
		return cfg.BridgeInterface
	}
	return ""
}

func summarizeErr(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	const maxLen = 64
	if len(msg) > maxLen {
		return msg[:maxLen-3] + "..."
	}
	return msg
}
