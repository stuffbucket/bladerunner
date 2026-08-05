//go:build darwin

package vm

import (
	"path/filepath"
	"testing"

	"github.com/Code-Hex/vz/v3"
	"github.com/stuffbucket/bladerunner/internal/config"
)

// newMinimalVZConfig builds a bare-bones, valid VZ configuration (EFI bootloader
// + min CPU/mem) sufficient to exercise the device-config helpers in isolation.
// The EFI variable store is created under t.TempDir so the test is hermetic.
func newMinimalVZConfig(t *testing.T) *vz.VirtualMachineConfiguration {
	t.Helper()
	varsPath := filepath.Join(t.TempDir(), "efi-vars.bin")
	store, err := vz.NewEFIVariableStore(varsPath, vz.WithCreatingEFIVariableStore())
	if err != nil {
		t.Fatalf("create efi variable store: %v", err)
	}
	bootLoader, err := vz.NewEFIBootLoader(vz.WithEFIVariableStore(store))
	if err != nil {
		t.Fatalf("create efi bootloader: %v", err)
	}
	cfg, err := vz.NewVirtualMachineConfiguration(
		bootLoader,
		vz.VirtualMachineConfigurationMinimumAllowedCPUCount(),
		vz.VirtualMachineConfigurationMinimumAllowedMemorySize(),
	)
	if err != nil {
		t.Fatalf("create vm configuration: %v", err)
	}
	return cfg
}

// TestConfigureShareAddsDevice verifies that configureShare builds the full
// VirtioFS device chain (SharedDirectory -> SingleDirectoryShare ->
// VirtioFileSystemDeviceConfiguration) and attaches it to the VZ configuration
// without error when ShareDir is set. The vz API exposes no getter for the
// directory-sharing device list, so success of the real device-build chain
// (each step returns a live objc object or this errors) is the assertion.
func TestConfigureShareAddsDevice(t *testing.T) {
	shareDir := t.TempDir()
	r := &Runner{cfg: &config.Config{ShareDir: shareDir, ShareTag: config.DefaultShareTag}}

	cfg := newMinimalVZConfig(t)
	if err := r.configureShare(cfg); err != nil {
		t.Fatalf("configureShare with a valid share dir: %v", err)
	}

	// A second call with the default-tag path (empty ShareTag) must also succeed,
	// proving effectiveShareTag's default feeds a valid non-empty VZ tag.
	r2 := &Runner{cfg: &config.Config{ShareDir: shareDir}}
	if err := r2.configureShare(newMinimalVZConfig(t)); err != nil {
		t.Fatalf("configureShare with default tag: %v", err)
	}
}

// TestConfigureShareHonorsReadOnly is the Darwin half of #203. configureShare
// used to pass a hardcoded false to vz.NewSharedDirectory, so a cartridge
// manifest asking for a read-only share got a read-write VirtioFS export and
// the guest could write into the host directory.
//
// The vz API exposes no getter for a SharedDirectory's read-only flag, so the
// value handed to it is asserted through effectiveShareReadOnly — the single
// source of truth configureShare reads — and the device chain is then built for
// real with that value to prove VZ accepts it. Whether the guest is actually
// refused a write needs a booted VM; that is the smoke test's job.
func TestConfigureShareHonorsReadOnly(t *testing.T) {
	shareDir := t.TempDir()
	tests := []struct {
		name string
		cfg  *config.Config
		want bool
	}{
		{"the default cartridge share is read-write",
			&config.Config{ShareDir: shareDir, ShareTag: config.DefaultShareTag}, false},
		{"an explicit read-only share is read-only",
			&config.Config{ShareDir: shareDir, ShareTag: config.DefaultShareTag, ShareReadOnly: true}, true},
		{"no share means nothing to make read-only",
			&config.Config{ShareReadOnly: true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Runner{cfg: tt.cfg}
			if got := r.effectiveShareReadOnly(); got != tt.want {
				t.Fatalf("effectiveShareReadOnly = %v, want %v", got, tt.want)
			}
			if tt.cfg.ShareDir == "" {
				return // configureShare is never reached with no share dir
			}
			if err := r.configureShare(newMinimalVZConfig(t)); err != nil {
				t.Fatalf("configureShare: %v", err)
			}
		})
	}
}

// TestConfigureShareSkippedWhenEmpty verifies newVMConfiguration's gate: with
// ShareDir empty, configureShare is never invoked, so no device is added. We
// assert the gate condition directly (newVMConfiguration needs far more setup),
// mirroring the additive "empty => no device" contract.
func TestConfigureShareSkippedWhenEmpty(t *testing.T) {
	r := &Runner{cfg: &config.Config{ShareDir: ""}}
	if r.cfg.ShareDir != "" {
		t.Fatal("precondition: ShareDir must be empty")
	}
	// effectiveShareTag is the single source of truth for whether a device would
	// be built; empty ShareDir => empty tag => no device.
	if got := r.effectiveShareTag(); got != "" {
		t.Errorf("effectiveShareTag with empty ShareDir = %q, want empty", got)
	}

	// And when ShareDir is set but ShareTag is empty, the default tag is used.
	r2 := &Runner{cfg: &config.Config{ShareDir: t.TempDir()}}
	if got := r2.effectiveShareTag(); got != config.DefaultShareTag {
		t.Errorf("effectiveShareTag default = %q, want %q", got, config.DefaultShareTag)
	}
}
