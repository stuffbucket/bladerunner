package main

import (
	"path/filepath"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/cartridge"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/disk"
)

func TestClassifyBootArgCartridge(t *testing.T) {
	exists := func(p string) bool {
		switch p {
		case "/tmp/demo.sparseimage", "/tmp/demo.dmg", "/tmp/real.disk":
			return true
		}
		return false
	}
	tests := []struct {
		name string
		arg  string
		want bootTargetKind
	}{
		{"sparseimage", "/tmp/demo.sparseimage", bootTargetCartridge},
		{"dmg", "/tmp/demo.dmg", bootTargetCartridge},
		{"missing-sparseimage-falls-to-name", "/tmp/missing.sparseimage", bootTargetName},
		{"disk-still-file", "/tmp/real.disk", bootTargetFile},
		{"url-still-url", "https://x/y-arm64.qcow2", bootTargetURL},
		{"plain-name", "incus", bootTargetName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyBootArg(tt.arg, exists).kind; got != tt.want {
				t.Fatalf("classifyBootArg(%q).kind = %v, want %v", tt.arg, got, tt.want)
			}
		})
	}
}

func TestPackSizeGiB(t *testing.T) {
	// Explicit --size wins.
	if got := packSizeGiB(40, 20); got != 40 {
		t.Errorf("explicit size should win: got %d", got)
	}
	// Else disk + headroom (clamped to the cartridge minimum).
	if got := packSizeGiB(0, 20); got != cartridge.SizeGiB(20) {
		t.Errorf("default size = %d, want %d", got, cartridge.SizeGiB(20))
	}
	if got := packSizeGiB(0, 0); got != cartridge.MinSizeGiB {
		t.Errorf("zero disk should clamp to MinSizeGiB: got %d", got)
	}
}

func TestPackOutPath(t *testing.T) {
	// Explicit --out wins verbatim.
	if got, err := packOutPath("/x/custom.sparseimage", "demo"); err != nil || got != "/x/custom.sparseimage" {
		t.Fatalf("packOutPath(out) = %q, %v", got, err)
	}
	// Default is ./<name>.sparseimage in the cwd.
	got, err := packOutPath("", "demo")
	if err != nil {
		t.Fatalf("packOutPath default: %v", err)
	}
	if filepath.Base(got) != "demo"+cartridge.SparseExt {
		t.Fatalf("default out base = %q, want demo%s", filepath.Base(got), cartridge.SparseExt)
	}
}

func TestCartridgeMountpoint(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("BLADERUNNER_STATE_DIR", stateDir)
	want := filepath.Join(stateDir, "mnt", "demo")
	if got := cartridgeMountpoint("demo"); got != want {
		t.Fatalf("cartridgeMountpoint = %q, want %q", got, want)
	}
}

// TestApplyBootCartridgeDelegatesToOpenCartridge covers the CLI-side handoff:
// the per-boot state is an *cartridge.Opened value, and applyBootCartridge
// simply routes cfg through it. The rooting rules themselves are asserted in
// internal/cartridge (TestOpenedApplyToRootsConfigInsideMount).
func TestApplyBootCartridgeDelegatesToOpenCartridge(t *testing.T) {
	t.Cleanup(func() { bootCartridge.opened = nil; bootCartridge.mountpoint = "" })

	mp := "/state/mnt/demo"
	bootCartridge.opened = &cartridge.Opened{
		Name:     "demo",
		Mount:    cartridge.Mount{Mountpoint: mp},
		Layout:   cartridge.NewLayout(mp),
		Manifest: &disk.Manifest{Share: &disk.ShareSpec{Tag: "custom-tag"}},
	}
	bootCartridge.mountpoint = mp

	cfg := &config.Config{BaseImageURL: "https://should-be-cleared"}
	applyBootCartridge(cfg)

	if cfg.DiskPath != filepath.Join(mp, cartridge.RootImageFile) {
		t.Errorf("DiskPath = %q", cfg.DiskPath)
	}
	if cfg.BaseImageURL != "" {
		t.Errorf("BaseImageURL should be cleared, got %q", cfg.BaseImageURL)
	}
	if cfg.ShareTag != "custom-tag" {
		t.Errorf("ShareTag = %q, want custom-tag", cfg.ShareTag)
	}
}

func TestApplyBootCartridgeNoOpWhenNoCartridge(t *testing.T) {
	t.Cleanup(func() { bootCartridge.opened = nil; bootCartridge.mountpoint = "" })
	bootCartridge.opened = nil
	bootCartridge.mountpoint = ""
	cfg := &config.Config{BaseImageURL: "https://keep", ShareDir: ""}
	applyBootCartridge(cfg)
	if cfg.BaseImageURL != "https://keep" || cfg.ShareDir != "" {
		t.Errorf("applyBootCartridge mutated config for non-cartridge boot: %+v", cfg)
	}
}

// TestDetachBootCartridgeNoOpWhenNoCartridge guards the plain `br start` path:
// runStart always defers detachBootCartridge, which must do nothing (and not
// panic) when no cartridge was opened.
func TestDetachBootCartridgeNoOpWhenNoCartridge(t *testing.T) {
	t.Cleanup(func() { bootCartridge.opened = nil; bootCartridge.mountpoint = "" })
	bootCartridge.opened = nil
	bootCartridge.mountpoint = ""
	detachBootCartridge()
}
