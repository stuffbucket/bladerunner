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
	// a .dmg output is refused by packOutPath itself (see
	// TestPackOutPathRejectsANonSparseExtension) rather than silently producing
	// demo.dmg.sparseimage under the unusable name "demo.dmg".
	if resolved, err := packOutPath("/x/demo"+cartridge.DMGExt, "incus"); err == nil {
		t.Fatalf("packOutPath(.dmg) = %q, want a refusal", resolved)
	}
}

// `br disk pack incus --out demo.dmg` used to be accepted here, appended
// .sparseimage (hdiutil does that regardless), derive the cartridge name
// "demo.dmg" from the result and die three calls later on instance.ValidName's
// regex. --ship advertises a .dmg, so .dmg is exactly what a user reaches for;
// the extension is now refused up front, in a message that names both right
// answers instead of a regex.
func TestPackOutPathRejectsANonSparseExtension(t *testing.T) {
	for _, out := range []string{
		"demo" + cartridge.DMGExt,
		"/x/demo" + cartridge.DMGExt,
		"/x/demo.img",
		"/x/demo.sparsebundle",
	} {
		got, err := packOutPath(out, "incus")
		if err == nil {
			t.Errorf("packOutPath(%q) = %q, want a refusal", out, got)
			continue
		}
		if !errors.Is(err, errPackOutExtension) {
			t.Errorf("packOutPath(%q) error = %v, want it to wrap errPackOutExtension", out, err)
		}
		// The message has to carry the offending path and both fixes: the
		// runnable extension, and --ship for the AirDrop artifact.
		for _, want := range []string{out, cartridge.SparseExt, "--ship", cartridge.DMGExt} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("packOutPath(%q) error %q does not mention %q", out, err, want)
			}
		}
		// And it must not be the name regex the user cannot act on.
		if strings.Contains(err.Error(), nameRulePattern) {
			t.Errorf("packOutPath(%q) error %q still surfaces the name regex", out, err)
		}
	}
}

// nameRulePattern is instance.ValidName's regex, which is what the old failure
// put in front of the user.
const nameRulePattern = `^[a-z0-9][a-z0-9-]*$`

// A bare --out with no extension is still accepted and gets .sparseimage — the
// behavior the flag help now states.
func TestPackOutPathAcceptsABareName(t *testing.T) {
	got, err := packOutPath("/x/demo", "incus")
	if err != nil {
		t.Fatalf("packOutPath(bare): %v", err)
	}
	if want := "/x/demo" + cartridge.SparseExt; got != want {
		t.Fatalf("packOutPath(bare) = %q, want %q", got, want)
	}
	if usage := diskPackCmd.Flags().Lookup("out").Usage; !strings.Contains(usage, cartridge.SparseExt) {
		t.Errorf("--out help %q does not state the required extension", usage)
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

// --- --private-mount and --persist ---------------------------------------

// The browsable default is what Finder-eject (design goal 5) depends on, so it
// must survive the arrival of the flag that opts out of it.
func TestCartridgeOpenOptionsDefaultsToBrowsable(t *testing.T) {
	opts := cartridgeOpenOptions("demo", false, false)
	if !opts.Policy.Browsable() {
		t.Fatalf("Policy = %q, want the browsable default", opts.Policy)
	}
	if opts.Mountpoint != "" {
		t.Errorf("a browsable attach must dictate no mountpoint, got %q", opts.Mountpoint)
	}
	if opts.Persist {
		t.Errorf("Persist must default to false: booting a .dmg discards by default")
	}
}

// --private-mount attaches -nobrowse at the dictated <state>/mnt/<name>.
func TestCartridgeOpenOptionsPrivateMountDictatesTheMountpoint(t *testing.T) {
	t.Setenv("BLADERUNNER_STATE_DIR", t.TempDir())
	opts := cartridgeOpenOptions("demo", true, false)
	if !opts.Policy.Private() {
		t.Fatalf("Policy = %q, want private", opts.Policy)
	}
	if want := cartridgeMountpoint("demo"); opts.Mountpoint != want {
		t.Errorf("Mountpoint = %q, want %q", opts.Mountpoint, want)
	}
}

func TestCartridgeOpenOptionsPersistIsOptIn(t *testing.T) {
	if opts := cartridgeOpenOptions("demo", false, true); !opts.Persist {
		t.Fatalf("--persist did not reach OpenOptions: %+v", opts)
	}
}

// Both flags are cartridge-only. Silently ignoring one on a disk boot would
// tell the user their changes are being kept when nothing of the sort happens.
func TestCartridgeOnlyFlagsAreRefusedOnADiskBoot(t *testing.T) {
	for _, name := range cartridgeOnlyFlags {
		err := cartridgeOnlyFlagError(func(n string) bool { return n == name })
		if err == nil {
			t.Fatalf("--%s on a disk boot was accepted", name)
		}
		if !strings.Contains(err.Error(), "--"+name) {
			t.Errorf("error %q does not name --%s", err, name)
		}
	}
	if err := cartridgeOnlyFlagError(func(string) bool { return false }); err != nil {
		t.Errorf("a plain disk boot must not be refused: %v", err)
	}
}

// The flags have to be discoverable and unambiguous in `br boot --help`.
func TestBootHelpDocumentsTheCartridgeFlags(t *testing.T) {
	for _, name := range cartridgeOnlyFlags {
		f := bootCmd.Flags().Lookup(name)
		if f == nil {
			t.Fatalf("br boot has no --%s flag", name)
		}
		if f.DefValue != "false" {
			t.Errorf("--%s defaults to %q, want false", name, f.DefValue)
		}
		if len(f.Usage) < 40 {
			t.Errorf("--%s usage %q is too terse to be unambiguous", name, f.Usage)
		}
	}
	long := bootCmd.Long
	for _, want := range []string{"--persist", "--private-mount", "discard"} {
		if !strings.Contains(long, want) {
			t.Errorf("br boot --help does not explain %q", want)
		}
	}
}

// TestCleanUpPackKeepsAnImageItCouldNotRelease is the pack-side half of the
// unlink-safety invariant.
//
// `br disk pack` attaches the image it is building, so a failed pack has to
// release the volume before it may delete the partial file. The cleanup used to
// ignore the detach result entirely and unlink unconditionally, which is an
// unlink of a live mount's backing store whenever the detach failed — the same
// data loss the cartridge package refuses on the boot path.
func TestCleanUpPackKeepsAnImageItCouldNotRelease(t *testing.T) {
	const mountpoint = "/state/mnt/demo"
	img := filepath.Join(t.TempDir(), "demo"+cartridge.SparseExt)
	if err := os.WriteFile(img, []byte("partial-cartridge"), 0o600); err != nil {
		t.Fatalf("write image: %v", err)
	}
	release := func() error { return errors.New(`hdiutil: couldn't unmount "disk9" - Resource busy`) }

	err := cleanUpPack(release, img, mountpoint, false)
	if _, statErr := os.Stat(img); statErr != nil {
		t.Fatalf("the partial image was unlinked while it may still be attached: %v", statErr)
	}
	if err == nil {
		t.Fatal("a release that failed must be reported, not discarded")
	}
	// A cleanup failure the user cannot act on is its own bug: name the file
	// that was kept and the volume they have to eject.
	for _, want := range []string{img, mountpoint} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// The guard must not break the case it guards: once the release is CONFIRMED,
// the partial image is removed so a retry starts clean.
func TestCleanUpPackRemovesAPartialImageOnceReleased(t *testing.T) {
	img := filepath.Join(t.TempDir(), "demo"+cartridge.SparseExt)
	if err := os.WriteFile(img, []byte("partial-cartridge"), 0o600); err != nil {
		t.Fatalf("write image: %v", err)
	}
	if err := cleanUpPack(func() error { return nil }, img, "/state/mnt/demo", false); err != nil {
		t.Fatalf("cleanUpPack: %v", err)
	}
	if _, err := os.Stat(img); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the partial image survived a confirmed release: %v", err)
	}
}

// A pack that SUCCEEDED keeps its output, and a release failure after it is
// still reported — the cartridge is written, but a volume is still attached.
func TestCleanUpPackKeepsAPackedCartridge(t *testing.T) {
	img := filepath.Join(t.TempDir(), "demo"+cartridge.SparseExt)
	if err := os.WriteFile(img, []byte("packed-cartridge"), 0o600); err != nil {
		t.Fatalf("write image: %v", err)
	}
	if err := cleanUpPack(func() error { return nil }, img, "/state/mnt/demo", true); err != nil {
		t.Fatalf("cleanUpPack: %v", err)
	}
	if _, err := os.Stat(img); err != nil {
		t.Fatalf("a packed cartridge was removed: %v", err)
	}

	err := cleanUpPack(func() error { return errors.New("detach failed") }, img, "/state/mnt/demo", true)
	if err == nil {
		t.Fatal("a release failure after a successful pack must still be reported")
	}
	if _, statErr := os.Stat(img); statErr != nil {
		t.Fatalf("the packed cartridge was removed by the cleanup: %v", statErr)
	}
}
