package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/cartridge"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/disk"
	"github.com/stuffbucket/bladerunner/internal/instance"
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

// `br boot` must refuse a cartridge that is already running, and it must
// recognize it however the user spelled it: the shipped .dmg and the working
// .sparseimage the holder converted are one cartridge.
//
// The old guard probed a control socket under <state>/mnt/<name>, which a
// browsable cartridge never occupies — so it never fired, and the second boot
// went on to unlink the first VM's live disk.
func TestEnsureCartridgeBootableRefusesARunningCartridge(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BLADERUNNER_STATE_DIR", root)
	downloads := t.TempDir()
	source := filepath.Join(downloads, "demo"+cartridge.DMGExt)
	working := filepath.Join(downloads, "demo"+cartridge.SparseExt)

	if err := instance.Write(root, instance.Entry{
		Name:        "demo",
		Kind:        instance.KindCartridge,
		StateDir:    filepath.Join(cartridge.VolumesRoot, cartridge.VolumeName("demo")),
		Mountpoint:  filepath.Join(cartridge.VolumesRoot, cartridge.VolumeName("demo")),
		SourcePath:  source,
		WorkingCopy: working,
		PID:         os.Getpid(), // this test process is certainly alive
	}); err != nil {
		t.Fatalf("write registry entry: %v", err)
	}

	for _, spelling := range []string{source, working} {
		err := ensureCartridgeBootable(spelling, "demo")
		if !errors.Is(err, errCartridgeAlreadyBooted) {
			t.Fatalf("ensureCartridgeBootable(%q) = %v, want errCartridgeAlreadyBooted", spelling, err)
		}
		// The refusal has to say what to do about it.
		if !strings.Contains(err.Error(), "br eject demo") {
			t.Errorf("error %q does not say how to release the cartridge", err)
		}
	}

	// An unrelated cartridge is unaffected.
	if err := ensureCartridgeBootable(filepath.Join(downloads, "other"+cartridge.DMGExt), "other"); err != nil {
		t.Fatalf("an unrelated cartridge was refused: %v", err)
	}
}

// A dead holder's entry must not block a boot: the registry is advisory and
// crash-tolerant.
func TestEnsureCartridgeBootableIgnoresADeadHolder(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BLADERUNNER_STATE_DIR", root)
	downloads := t.TempDir()
	source := filepath.Join(downloads, "demo"+cartridge.DMGExt)

	if err := instance.Write(root, instance.Entry{
		Name:       "demo",
		Kind:       instance.KindCartridge,
		StateDir:   filepath.Join(root, "gone"),
		SourcePath: source,
		PID:        -1, // no such process, and no control socket either
	}); err != nil {
		t.Fatalf("write registry entry: %v", err)
	}

	if err := ensureCartridgeBootable(source, "demo"); err != nil {
		t.Fatalf("a dead holder blocked the boot: %v", err)
	}
}

// `br disks` lists a booted cartridge. It is mounted under /Volumes now, so the
// <state>/mnt scan that used to be the only source always came back empty and
// the "cartridges" section was permanently absent.
func TestListAttachedCartridgesSeesRegisteredCartridges(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BLADERUNNER_STATE_DIR", root)

	if got := listAttachedCartridges(); got != nil {
		t.Fatalf("nothing attached must stay nil for --json, got %+v", got)
	}

	mount := filepath.Join(cartridge.VolumesRoot, cartridge.VolumeName("demo"))
	for _, e := range []instance.Entry{
		{
			Name: "demo", Kind: instance.KindCartridge,
			StateDir: mount, Mountpoint: mount, PID: os.Getpid(),
		},
		// A disk slot is not a cartridge and must not be listed as one.
		{Name: "builder", Kind: instance.KindDisk, StateDir: filepath.Join(root, "disks", "builder"), PID: os.Getpid()},
		// A dead cartridge holder is not attached either.
		{Name: "ghost", Kind: instance.KindCartridge, StateDir: filepath.Join(root, "gone"), PID: -1},
	} {
		if err := instance.Write(root, e); err != nil {
			t.Fatalf("write registry entry %q: %v", e.Name, err)
		}
	}

	got := listAttachedCartridges()
	if len(got) != 1 {
		t.Fatalf("listAttachedCartridges() = %+v, want exactly the booted cartridge", got)
	}
	if got[0].Name != "demo" || got[0].Mountpoint != mount {
		t.Errorf("cartridge = %+v, want demo at %q", got[0], mount)
	}
}
