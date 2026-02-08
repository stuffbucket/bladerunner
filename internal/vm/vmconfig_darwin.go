//go:build darwin

package vm

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/Code-Hex/vz/v3"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/util"
)

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
	if util.FileExists(path) {
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
	if util.FileExists(r.cfg.MachineIDPath) {
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
	cpu := max(requested, 1)
	maxCPU := vz.VirtualMachineConfigurationMaximumAllowedCPUCount()
	minCPU := vz.VirtualMachineConfigurationMinimumAllowedCPUCount()
	return max(min(cpu, maxCPU), minCPU)
}

func clampMemory(bytes uint64) uint64 {
	maxMem := vz.VirtualMachineConfigurationMaximumAllowedMemorySize()
	minMem := vz.VirtualMachineConfigurationMinimumAllowedMemorySize()
	mem := max(min(bytes, maxMem), minMem)

	// Must be in MiB increments.
	const mib = 1024 * 1024
	mem = (mem / mib) * mib
	return max(mem, minMem)
}
