//go:build darwin

// runtimeMetadata is loaded and persisted only by the darwin VM runner
// (runner_darwin.go); on other platforms the VM runner is an unsupported stub,
// so this file is darwin-tagged to keep it out of those builds.
package vm

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"os"

	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/util"
)

// breakGlassPasswordLen is the length of the generated per-instance break-glass
// SSH password. 24 characters from a 62-symbol alphabet is ~143 bits of entropy
// — far beyond brute force over the loopback-only vsock bridge.
const breakGlassPasswordLen = 24

// breakGlassPasswordAlphabet is the character set the per-instance password is
// drawn from. It deliberately excludes the single-quote (the shell literal
// delimiter in the bootstrap) and other shell/YAML metacharacters, so the
// password is safe to embed verbatim in the cloud-init chpasswd module and the
// bootstrap's single-quoted SSH_BREAK_GLASS_PW without any escaping.
const breakGlassPasswordAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

type runtimeMetadata struct {
	MACAddress string `json:"mac_address"`

	// SSHBreakGlassPassword is the per-instance random break-glass SSH password.
	// Primary access is the SSH key in authorized_keys; this is only a fallback
	// over the loopback-only vsock bridge. It is generated once and persisted so
	// it is stable for this VM across binary updates and can be surfaced to the
	// operator. Older VMs provisioned with the historical hardcoded literal are
	// unaffected: they never re-provision, so their guest password is unchanged
	// regardless of what this file holds.
	SSHBreakGlassPassword string `json:"ssh_break_glass_password,omitempty"`

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
	var md runtimeMetadata
	loaded := false
	if util.FileExists(cfg.MetadataPath) {
		b, err := os.ReadFile(cfg.MetadataPath)
		if err == nil {
			if err := json.Unmarshal(b, &md); err == nil {
				loaded = true
			} else {
				md = runtimeMetadata{}
			}
		}
	}

	// Ensure both the MAC and the break-glass password exist (load-or-create):
	// either may be absent on a fresh VM or an older metadata file that predates
	// the password field. Persist only when we actually filled something in, so a
	// complete existing file is never rewritten.
	dirty := false
	if md.MACAddress == "" {
		mac, err := generateLocalMAC()
		if err != nil {
			return nil, err
		}
		md.MACAddress = mac.String()
		dirty = true
	}
	if md.SSHBreakGlassPassword == "" {
		pw, err := generateBreakGlassPassword()
		if err != nil {
			return nil, err
		}
		md.SSHBreakGlassPassword = pw
		dirty = true
	}

	if dirty || !loaded {
		if err := saveMetadata(cfg, &md); err != nil {
			return nil, err
		}
	}
	return &md, nil
}

func saveMetadata(cfg *config.Config, md *runtimeMetadata) error {
	b, err := json.MarshalIndent(md, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal runtime metadata: %w", err)
	}
	// 0600: the metadata now carries the per-instance break-glass SSH password, so
	// keep it readable only by the owner.
	if err := os.WriteFile(cfg.MetadataPath, b, 0o600); err != nil {
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

// generateBreakGlassPassword returns a cryptographically-random break-glass SSH
// password drawn from breakGlassPasswordAlphabet. The alphabet has no shell/YAML
// metacharacters, so the result is safe to embed verbatim in the cloud-init
// chpasswd module and the single-quoted bootstrap variable.
func generateBreakGlassPassword() (string, error) {
	alphabetLen := big.NewInt(int64(len(breakGlassPasswordAlphabet)))
	buf := make([]byte, breakGlassPasswordLen)
	for i := range buf {
		n, err := rand.Int(rand.Reader, alphabetLen)
		if err != nil {
			return "", fmt.Errorf("generate break-glass password: %w", err)
		}
		buf[i] = breakGlassPasswordAlphabet[n.Int64()]
	}
	return string(buf), nil
}
