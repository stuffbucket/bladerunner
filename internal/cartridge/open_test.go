package cartridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/disk"
)

// openFixture lays out a complete, verifiable cartridge at dir — what the real
// hdiutil would have made appear at the mountpoint. The fake runner cannot
// mount anything, so the volume contents are staged up front.
func openFixture(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, layoutDirPerm); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := Pack(dir, validSourceManifest(), PackOptions{Name: fixtureCartridgeName, PackedBy: "br-test"}); err != nil {
		t.Fatalf("Pack: %v", err)
	}
	writeFixtureFile(t, filepath.Join(dir, RootImageFile), "not-really-a-disk")
}

// openTestDevNode is the BSD device the fake hdiutil claims to have attached.
const openTestDevNode = "/dev/disk9s1"

// fixtureCartridgeName is the name every staged cartridge is packed under.
const fixtureCartridgeName = "demo"

// attachResult scripts a successful `hdiutil attach -plist` for mountpoint.
func attachResult(mountpoint string) fakeResult {
	return fakeResult{stdout: attachPlistFor(resolvePath(mountpoint), openTestDevNode)}
}

func TestOpenAttachesAndLoadsTheCartridge(t *testing.T) {
	mp := filepath.Join(t.TempDir(), "mnt", "demo")
	openFixture(t, mp)
	f := &fakeRunner{results: []fakeResult{attachResult(mp)}}

	o, err := open(context.Background(), f, "/images/demo"+SparseExt, OpenOptions{Mountpoint: mp})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if o.Name != "demo" {
		t.Errorf("Name = %q, want demo", o.Name)
	}
	if o.SourcePath != "/images/demo"+SparseExt {
		t.Errorf("SourcePath = %q", o.SourcePath)
	}
	if o.WorkingCopy != "" {
		t.Errorf("a runnable .sparseimage needs no working copy, got %q", o.WorkingCopy)
	}
	if o.Mount.DevNode != openTestDevNode {
		t.Errorf("Mount.DevNode = %q, want %q", o.Mount.DevNode, openTestDevNode)
	}
	if o.Layout.Mountpoint != resolvePath(mp) {
		t.Errorf("Layout.Mountpoint = %q, want %q", o.Layout.Mountpoint, resolvePath(mp))
	}
	if o.Manifest == nil || o.Manifest.Name != "demo" {
		t.Fatalf("Manifest = %+v", o.Manifest)
	}
	if o.Metadata.FormatVersion != FormatVersion {
		t.Errorf("Metadata.FormatVersion = %d", o.Metadata.FormatVersion)
	}
	// Exactly one hdiutil call: the attach. No conversion, no detach.
	if len(f.calls) != 1 || f.calls[0][1] != cmdAttach {
		t.Fatalf("hdiutil calls = %v, want a single attach", f.calls)
	}
}

func TestOpenNameOverrideWins(t *testing.T) {
	mp := filepath.Join(t.TempDir(), "mnt", "slot")
	openFixture(t, mp)
	f := &fakeRunner{results: []fakeResult{attachResult(mp)}}

	o, err := open(context.Background(), f, "/images/demo"+SparseExt, OpenOptions{Mountpoint: mp, Name: "slot"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if o.Name != "slot" {
		t.Fatalf("Name = %q, want slot", o.Name)
	}
}

// TestOpenConvertsAShippedDMG pins the pristine-artifact rule: a .dmg is never
// attached directly, it is converted to a writable working .sparseimage first.
func TestOpenConvertsAShippedDMG(t *testing.T) {
	tmp := t.TempDir()
	mp := filepath.Join(tmp, "mnt", "demo")
	openFixture(t, mp)
	dmg := filepath.Join(tmp, "demo"+DMGExt)
	f := &fakeRunner{results: []fakeResult{{}, attachResult(mp)}}

	o, err := open(context.Background(), f, dmg, OpenOptions{Mountpoint: mp})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	want := filepath.Join(tmp, "demo"+SparseExt)
	if o.WorkingCopy != want {
		t.Fatalf("WorkingCopy = %q, want %q", o.WorkingCopy, want)
	}
	if len(f.calls) != 2 {
		t.Fatalf("hdiutil calls = %v, want convert then attach", f.calls)
	}
	convertCall := f.calls[0]
	if convertCall[1] != cmdConvert || convertCall[2] != dmg {
		t.Errorf("first call = %v, want a convert of the dmg", convertCall)
	}
	if !argsEqual(convertCall, []string{hdiutil, cmdConvert, dmg, flagFormat, formatUDSP, "-o", filepath.Join(tmp, "demo"), flagQuiet}) {
		t.Errorf("convert args = %v", convertCall)
	}
	// The attach must target the working copy, never the shipped dmg.
	if f.calls[1][2] != want {
		t.Errorf("attach target = %q, want the working copy %q", f.calls[1][2], want)
	}
}

func TestOpenRequiresAMountpoint(t *testing.T) {
	_, err := open(context.Background(), &fakeRunner{}, "/images/demo"+SparseExt, OpenOptions{})
	if !errors.Is(err, ErrNoMountpoint) {
		t.Fatalf("err = %v, want ErrNoMountpoint", err)
	}
}

// TestOpenRejectsANonCartridgeAndUnwinds is the reason Open verifies: attaching
// an arbitrary image must not leave it mounted once we know it is not bootable.
func TestOpenRejectsANonCartridgeAndUnwinds(t *testing.T) {
	mp := filepath.Join(t.TempDir(), "mnt", "demo")
	if err := os.MkdirAll(mp, layoutDirPerm); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f := &fakeRunner{results: []fakeResult{attachResult(mp)}}

	_, err := open(context.Background(), f, "/images/demo"+SparseExt, OpenOptions{Mountpoint: mp})
	if !errors.Is(err, ErrNotCartridge) {
		t.Fatalf("err = %v, want ErrNotCartridge", err)
	}
	if len(f.calls) != 2 || f.calls[1][1] != cmdDetach {
		t.Fatalf("hdiutil calls = %v, want the attach to be unwound by a detach", f.calls)
	}
}

func TestOpenRejectsAFutureFormatAndUnwinds(t *testing.T) {
	mp := filepath.Join(t.TempDir(), "mnt", "demo")
	openFixture(t, mp)
	writeFormatVersion(t, mp, FormatVersion+1)
	f := &fakeRunner{results: []fakeResult{attachResult(mp)}}

	_, err := open(context.Background(), f, "/images/demo"+SparseExt, OpenOptions{Mountpoint: mp})
	if !errors.Is(err, ErrFormatTooNew) {
		t.Fatalf("err = %v, want ErrFormatTooNew", err)
	}
	if len(f.calls) != 2 || f.calls[1][1] != cmdDetach {
		t.Fatalf("hdiutil calls = %v, want the attach to be unwound by a detach", f.calls)
	}
}

// TestOpenRemovesTheWorkingCopyWhenAttachFails guards the other unwind: a
// conversion that succeeded must not leave a gigabyte behind when the attach
// then failed.
func TestOpenRemovesTheWorkingCopyWhenAttachFails(t *testing.T) {
	tmp := t.TempDir()
	dmg := filepath.Join(tmp, "demo"+DMGExt)
	work := filepath.Join(tmp, "demo"+SparseExt)
	writeFixtureFile(t, work, "converted-bytes")
	f := &fakeRunner{results: []fakeResult{
		{},
		{stderr: "hdiutil: attach failed", err: errors.New("exit 1")},
	}}

	_, err := open(context.Background(), f, dmg, OpenOptions{Mountpoint: filepath.Join(tmp, "mnt")})
	if err == nil {
		t.Fatal("expected an attach error")
	}
	if _, statErr := os.Stat(work); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("working copy %s should have been removed: %v", work, statErr)
	}
}

// TestOpenedCloseDetachesThenRemovesTheWorkingCopy pins the teardown ORDER: the
// working copy is the mount's backing store, so it can only go once the volume
// is detached.
func TestOpenedCloseDetachesThenRemovesTheWorkingCopy(t *testing.T) {
	tmp := t.TempDir()
	work := filepath.Join(tmp, "demo"+SparseExt)
	writeFixtureFile(t, work, "converted-bytes")
	o := &Opened{
		Name:        "demo",
		SourcePath:  filepath.Join(tmp, "demo"+DMGExt),
		WorkingCopy: work,
		Mount:       Mount{Mountpoint: filepath.Join(tmp, "mnt"), DevNode: openTestDevNode},
	}
	f := &fakeRunner{}

	if err := o.closeWith(context.Background(), f); err != nil {
		t.Fatalf("close: %v", err)
	}
	if len(f.calls) != 1 || f.calls[0][1] != cmdDetach {
		t.Fatalf("hdiutil calls = %v, want a single detach", f.calls)
	}
	if _, err := os.Stat(work); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("working copy still present: %v", err)
	}

	// Idempotent: a second close neither detaches again nor errors.
	if err := o.closeWith(context.Background(), f); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("second close ran hdiutil again: %v", f.calls)
	}
}

func TestOpenedCloseReportsTheDetachError(t *testing.T) {
	o := &Opened{Mount: Mount{Mountpoint: "/state/mnt/demo"}}
	f := &fakeRunner{results: []fakeResult{{stderr: "hdiutil: detach failed - broken", err: errors.New("exit 1")}}}
	if err := o.closeWith(context.Background(), f); err == nil {
		t.Fatal("expected the detach error to surface")
	}
}

// TestOpenedApplyToRootsConfigInsideMount is the whole point of a cartridge:
// every per-VM path lands inside the mounted volume, and no host-side image
// identity survives.
func TestOpenedApplyToRootsConfigInsideMount(t *testing.T) {
	mp := "/state/mnt/demo"
	o := &Opened{
		Name:     "demo",
		Mount:    Mount{Mountpoint: mp},
		Layout:   NewLayout(mp),
		Manifest: &disk.Manifest{Share: &disk.ShareSpec{Tag: "custom-tag"}},
	}

	cfg := &config.Config{
		BaseImageURL:            "https://should-be-cleared",
		BaseImageSHA512:         "should-be-cleared",
		BaseImageExpectedSHA256: "should-be-cleared",
	}
	o.ApplyTo(cfg)

	root := filepath.Join(mp, RootImageFile)
	if cfg.BaseImagePath != root || cfg.DiskPath != root {
		t.Errorf("BaseImagePath = %q, DiskPath = %q, want %q", cfg.BaseImagePath, cfg.DiskPath, root)
	}
	if cfg.BaseImageURL != "" || cfg.BaseImageSHA512 != "" || cfg.BaseImageExpectedSHA256 != "" {
		t.Errorf("remote image identity survived: %+v", cfg)
	}
	if cfg.EFIVarsPath != filepath.Join(mp, StateDirName, EFIVarsFile) {
		t.Errorf("EFIVarsPath = %q", cfg.EFIVarsPath)
	}
	if cfg.CloudInitDir != filepath.Join(mp, StateDirName, CloudInitDirName) {
		t.Errorf("CloudInitDir = %q", cfg.CloudInitDir)
	}
	if cfg.ShareDir != filepath.Join(mp, ShareDirName) {
		t.Errorf("ShareDir = %q", cfg.ShareDir)
	}
	if cfg.ShareTag != "custom-tag" || cfg.ShareGuestPath != config.DefaultShareGuestPath {
		t.Errorf("share = %q at %q", cfg.ShareTag, cfg.ShareGuestPath)
	}
}

func TestOpenedApplyToIsANoOpWhenNothingIsOpen(t *testing.T) {
	cfg := &config.Config{BaseImageURL: "https://keep"}
	var none *Opened
	none.ApplyTo(cfg)
	(&Opened{}).ApplyTo(cfg)
	if cfg.BaseImageURL != "https://keep" || cfg.DiskPath != "" {
		t.Fatalf("config mutated with no cartridge open: %+v", cfg)
	}
}

func TestOpenedAccessors(t *testing.T) {
	var none *Opened
	if none.Mountpoint() != "" || none.GUI() || none.StillAttached() {
		t.Fatal("a nil cartridge must answer everything negatively")
	}
	o := &Opened{
		Mount:    Mount{Mountpoint: "/state/mnt/demo"},
		Manifest: &disk.Manifest{Boot: disk.BootSpec{Mode: disk.BootModeGUI}},
	}
	if o.Mountpoint() != "/state/mnt/demo" {
		t.Errorf("Mountpoint = %q", o.Mountpoint())
	}
	if !o.GUI() {
		t.Error("GUI() should follow the manifest boot mode")
	}
	// No dev node was captured, so identity cannot be asserted.
	if o.StillAttached() {
		t.Error("StillAttached must be false without a dev node")
	}
}

func TestOpenIsUnsupportedOffDarwin(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("hdiutil exists here; the unsupported path is not reachable")
	}
	if _, err := Open("/images/demo"+SparseExt, OpenOptions{Mountpoint: t.TempDir()}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Open off darwin = %v, want ErrUnsupported", err)
	}
	if err := (&Opened{}).Close(); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Close off darwin = %v, want ErrUnsupported", err)
	}
}
