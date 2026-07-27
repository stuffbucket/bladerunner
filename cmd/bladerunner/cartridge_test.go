package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/cartridge"
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
	// An --out without the extension gets it, because hdiutil appends it
	// anyway: the path we name the cartridge after has to be the file that
	// actually appears, or pack and boot derive different names for it.
	if got, err := packOutPath("/x/custom", "demo"); err != nil || got != "/x/custom"+cartridge.SparseExt {
		t.Fatalf("packOutPath(no ext) = %q, %v", got, err)
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

// Whatever --out is spelled, the name the cartridge is packed under and the
// name `br boot` derives from the file that appears must be the same string.
func TestPackOutPathAndCartridgeNameAgree(t *testing.T) {
	for _, out := range []string{"/x/demo" + cartridge.SparseExt, "/x/demo", "demo"} {
		resolved, err := packOutPath(out, "incus")
		if err != nil {
			t.Fatalf("packOutPath(%q): %v", out, err)
		}
		name, err := packCartridgeName(resolved)
		if err != nil {
			t.Fatalf("packCartridgeName(%q): %v", resolved, err)
		}
		if name != "demo" {
			t.Errorf("--out %q packed as %q, want demo", out, name)
		}
		if boot := cartridge.NameFromPath(resolved); boot != name {
			t.Errorf("--out %q: packed as %q but br boot derives %q", out, name, boot)
		}
	}
	// `disk pack` writes the runnable form; --ship writes the .dmg. Asking for
	// a .dmg output is refused rather than silently producing
	// demo.dmg.sparseimage under the unusable name "demo.dmg".
	resolved, err := packOutPath("/x/demo"+cartridge.DMGExt, "incus")
	if err != nil {
		t.Fatalf("packOutPath: %v", err)
	}
	if _, err := packCartridgeName(resolved); !errors.Is(err, instance.ErrInvalidName) {
		t.Fatalf("packCartridgeName(%q) = %v, want an invalid-name error", resolved, err)
	}
}

// TestPackCartridgeNameComesFromTheOutputFile is the CLI half of the volume-name
// regression (see internal/cartridge's
// TestCreateArgsVolumeNameComesFromTheCartridgeNotTheDisk): `br disk pack
// <disk> --out <file>` must name the cartridge after the FILE it writes, so the
// baked-in volume name, the on-image metadata and the name `br boot <file>`
// derives are one name rather than three.
//
// The cases are already-resolved paths (packOutPath ran first), which is the
// only form runDiskPack ever passes.
func TestPackCartridgeNameComesFromTheOutputFile(t *testing.T) {
	tests := []struct {
		name string
		out  string
		disk string
		want string
	}{
		{"out differs from the disk", "/tmp/smoke-cartridge" + cartridge.SparseExt, "debian-trixie-gui", "smoke-cartridge"},
		{"directories are not part of the name", "/a/b/c/handoff" + cartridge.SparseExt, "incus", "handoff"},
		{"digits and dashes survive", "/tmp/blue-2" + cartridge.SparseExt, "incus", "blue-2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := packCartridgeName(tt.out)
			if err != nil {
				t.Fatalf("packCartridgeName(%q): %v", tt.out, err)
			}
			if got != tt.want {
				t.Fatalf("packCartridgeName(%q) = %q, want %q (not the disk %q)", tt.out, got, tt.want, tt.disk)
			}
			// The volume `hdiutil create` will bake in must round-trip back to
			// the same name through mount detection...
			if back := cartridge.NameFromVolume(cartridge.VolumeName(got)); back != got {
				t.Errorf("NameFromVolume round trip = %q, want %q", back, got)
			}
			// ...and `br boot <file>` must derive that same name, since the
			// instance name, the registry key and the ssh alias all follow it.
			if boot := cartridge.NameFromPath(tt.out); boot != got {
				t.Errorf("br boot would call it %q, packed as %q", boot, got)
			}
		})
	}
}

// With --out omitted the output is ./<disk>.sparseimage, so the cartridge is
// still named after the disk — the pre-fix behavior, now reached by derivation
// rather than by assumption.
func TestPackCartridgeNameDefaultsToTheDiskName(t *testing.T) {
	out, err := packOutPath("", "debian-trixie-gui")
	if err != nil {
		t.Fatalf("packOutPath: %v", err)
	}
	got, err := packCartridgeName(out)
	if err != nil {
		t.Fatalf("packCartridgeName(%q): %v", out, err)
	}
	if got != "debian-trixie-gui" {
		t.Fatalf("packCartridgeName = %q, want debian-trixie-gui", got)
	}
}

// A cartridge name has to survive as far as the instance registry, so an --out
// whose base name cannot be one is refused at pack time — with the name and the
// path in the message — rather than producing a cartridge that boots into an
// unregistrable instance.
func TestPackCartridgeNameRejectsAnUnusableOutputName(t *testing.T) {
	for _, out := range []string{
		"/tmp/My Cartridge" + cartridge.SparseExt, // spaces and capitals
		"/tmp/-leading-dash" + cartridge.SparseExt,
		"/tmp/" + cartridge.SparseExt, // nothing but an extension
		"/tmp/über" + cartridge.SparseExt,
		"/tmp/" + strings.Repeat("x", instance.MaxNameLen+1) + cartridge.SparseExt,
	} {
		got, err := packCartridgeName(out)
		if err == nil {
			t.Errorf("packCartridgeName(%q) = %q, want an error", out, got)
			continue
		}
		if !errors.Is(err, instance.ErrInvalidName) {
			t.Errorf("packCartridgeName(%q) error = %v, want it to wrap instance.ErrInvalidName", out, err)
		}
		if !strings.Contains(err.Error(), out) {
			t.Errorf("error %q does not name the offending path %q", err, out)
		}
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

// The rooting rules a cartridge imposes on the config (root.img, state/,
// share/) are internal/cartridge's — TestOpenedApplyToRootsConfigInsideMount —
// and they are applied by the vmhost.Host, which takes the open cartridge from
// takeBootCartridge. This file used to carry a CLI-side re-assertion of the
// same thing through an applyBootCartridge shim no production path called; the
// shim (and its tests) are gone, and what remains asserted here is the handoff
// itself.

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
