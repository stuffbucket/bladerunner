//go:build darwin

// runtimeMetadata is loaded and persisted only by the darwin VM runner
// (runner_darwin.go); on other platforms the VM runner is an unsupported stub,
// so this file is darwin-tagged to keep it out of those builds.
package vm

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"os"

	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/util"
)

type runtimeMetadata struct {
	MACAddress string `json:"mac_address"`

	// GrubHardened is set true once the disk completes a fully-ready boot, which
	// proves the first-boot bootstrap's update-grub ran and installed the
	// recordfail-timeout-0 drop-in. Once true it never reverts: the on-disk
	// grub.cfg stays hardened across reboots.
	//
	// recordfail background: an unclean guest shutdown sets recordfail=1 in the
	// guest grubenv; only an un-hardened grub.cfg stalls the boot menu on that
	// flag. A hardened grub.cfg forces the recordfail timeout to 0, so it can
	// never stall.
	GrubHardened bool `json:"grub_hardened,omitempty"`

	// LastShutdownClean is true only when the previous run ended with a graceful
	// ACPI stop. It is armed false at boot start and set true again only if a
	// clean host teardown runs; a crash or kill -9 leaves it false, which
	// signals recordfail=1 is likely set in the guest grubenv.
	LastShutdownClean bool `json:"last_shutdown_clean,omitempty"`
}

// peekMetadata reads persisted runtime metadata for cfg without starting the
// VM (all-false zero value when absent — the conservative default).
func peekMetadata(cfg *config.Config) runtimeMetadata {
	var md runtimeMetadata
	if !util.FileExists(cfg.MetadataPath) {
		return md
	}
	b, err := os.ReadFile(cfg.MetadataPath)
	if err != nil {
		return runtimeMetadata{}
	}
	if err := json.Unmarshal(b, &md); err != nil {
		return runtimeMetadata{}
	}
	return md
}

func loadOrCreateMetadata(cfg *config.Config) (*runtimeMetadata, error) {
	if util.FileExists(cfg.MetadataPath) {
		b, err := os.ReadFile(cfg.MetadataPath)
		if err == nil {
			var md runtimeMetadata
			if err := json.Unmarshal(b, &md); err == nil && md.MACAddress != "" {
				return &md, nil
			}
		}
	}

	mac, err := generateLocalMAC()
	if err != nil {
		return nil, err
	}

	md := &runtimeMetadata{MACAddress: mac.String()}
	if err := saveMetadata(cfg, md); err != nil {
		return nil, err
	}
	return md, nil
}

func saveMetadata(cfg *config.Config, md *runtimeMetadata) error {
	b, err := json.MarshalIndent(md, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal runtime metadata: %w", err)
	}
	if err := os.WriteFile(cfg.MetadataPath, b, 0o644); err != nil {
		return fmt.Errorf("write runtime metadata: %w", err)
	}
	return nil
}

func generateLocalMAC() (net.HardwareAddr, error) {
	mac := make([]byte, 6)
	if _, err := rand.Read(mac); err != nil {
		return nil, fmt.Errorf("generate random mac: %w", err)
	}
	mac[0] = (mac[0] | 2) & 0xfe // locally administered unicast
	return net.HardwareAddr(mac), nil
}
