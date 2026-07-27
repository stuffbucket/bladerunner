package cartridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
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

// attachedImageResult scripts an `hdiutil info -plist` reporting imagePath as
// attached and mounted at mountpoint — the shape a cartridge left behind by a
// dead holder produces. The plist itself is built by detect_test.go's
// infoPlistFor, which drives the real parser.
func attachedImageResult(imagePath, devNode, mountpoint string) fakeResult {
	return fakeResult{stdout: infoPlistFor(mountpoint, devNode, imagePath, true)}
}

// privateOpen is the option set every legacy test uses: the pre-inversion
// behavior, where the caller dictates the mountpoint.
func privateOpen(mountpoint string) OpenOptions {
	return OpenOptions{Mountpoint: mountpoint, Policy: MountPrivate}
}

func TestOpenAttachesAndLoadsTheCartridge(t *testing.T) {
	tmp := t.TempDir()
	mp := filepath.Join(tmp, "mnt", "demo")
	openFixture(t, mp)
	image := filepath.Join(tmp, "demo"+SparseExt)
	f := &fakeRunner{results: []fakeResult{attachResult(mp)}}

	o, err := open(context.Background(), f, image, privateOpen(mp))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { o.releaseClaim() })
	if o.Name != "demo" {
		t.Errorf("Name = %q, want demo", o.Name)
	}
	if o.SourcePath != image {
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
	tmp := t.TempDir()
	mp := filepath.Join(tmp, "mnt", "slot")
	openFixture(t, mp)
	f := &fakeRunner{results: []fakeResult{attachResult(mp)}}

	o, err := open(context.Background(), f, filepath.Join(tmp, "demo"+SparseExt), OpenOptions{Mountpoint: mp, Name: "slot", Policy: MountPrivate})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { o.releaseClaim() })
	if o.Name != "slot" {
		t.Fatalf("Name = %q, want slot", o.Name)
	}
}

// TestOpenLegacyCartridgeWithADisagreeingVolumeName is the backward-compatibility
// half of the volume-name fix.
//
// A cartridge packed by an OLDER build carries the SOURCE DISK's name in its
// APFS volume and in its on-image metadata, whichever file it was written to:
// `br disk pack debian-trixie-gui --out smoke-cartridge.sparseimage` produced a
// volume named bladerunner-debian-trixie-gui inside smoke-cartridge.sparseimage.
// Those cartridges are already in circulation (they AirDrop), so opening and
// booting one must keep working exactly as before — the new derivation applies
// at PACK time only, and nothing on the open path may start requiring the two
// names to agree.
func TestOpenLegacyCartridgeWithADisagreeingVolumeName(t *testing.T) {
	tmp := t.TempDir()
	// The mount is named after the disk (what the old -volname baked in) while
	// the image file is named after the cartridge — the disagreement itself.
	mp := filepath.Join(tmp, VolumesRoot, VolumeName("debian-trixie-gui"))
	if err := os.MkdirAll(mp, layoutDirPerm); err != nil {
		t.Fatalf("mkdir %s: %v", mp, err)
	}
	// Packed the old way: the metadata and manifest carry the disk's name too.
	if err := Pack(mp, validSourceManifest(), PackOptions{Name: "debian-trixie-gui", PackedBy: "br-old"}); err != nil {
		t.Fatalf("Pack: %v", err)
	}
	writeFixtureFile(t, filepath.Join(mp, RootImageFile), "not-really-a-disk")

	image := filepath.Join(tmp, "smoke-cartridge"+SparseExt)
	f := &fakeRunner{results: []fakeResult{attachResult(mp)}}

	o, err := open(context.Background(), f, image, privateOpen(mp))
	if err != nil {
		t.Fatalf("a cartridge from an older build must still open: %v", err)
	}
	t.Cleanup(func() { o.releaseClaim() })

	// The boot-side name still comes from the FILE, as it always has; the stale
	// volume name is simply never consulted.
	if o.Name != "smoke-cartridge" {
		t.Errorf("Name = %q, want smoke-cartridge (derived from the file)", o.Name)
	}
	// ...and the cartridge's own record of what it was packed as is preserved,
	// not overwritten or rejected.
	if o.Metadata.Name != "debian-trixie-gui" {
		t.Errorf("Metadata.Name = %q, want the name it was packed under", o.Metadata.Name)
	}
	if o.Manifest == nil || o.Manifest.Name != "debian-trixie-gui" {
		t.Fatalf("Manifest = %+v, want the packed manifest untouched", o.Manifest)
	}

	// Detection of that same legacy volume also still works: the prefilter only
	// asks for the bladerunner- prefix, and the name it reports is the one the
	// cartridge records.
	if !IsCandidate(filepath.Base(mp)) {
		t.Errorf("IsCandidate(%q) = false; a legacy cartridge would never be offered", filepath.Base(mp))
	}
	d := detectNoHdiutil(t, mp)
	if d.Status != StatusBootable {
		t.Fatalf("Status = %q (%s), want %q", d.Status, d.Reason, StatusBootable)
	}
	if d.Name != "debian-trixie-gui" {
		t.Errorf("Detected.Name = %q, want debian-trixie-gui", d.Name)
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

	o, err := open(context.Background(), f, dmg, privateOpen(mp))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { o.releaseClaim() })
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

// TestOpenRequiresAMountpoint applies to the PRIVATE policy only: there the
// caller dictates the location, so omitting it is a caller bug. The browsable
// default needs no mountpoint at all (see TestOpenBrowsableNeedsNoMountpoint).
func TestOpenRequiresAMountpoint(t *testing.T) {
	_, err := open(context.Background(), &fakeRunner{}, filepath.Join(t.TempDir(), "demo"+SparseExt), OpenOptions{Policy: MountPrivate})
	if !errors.Is(err, ErrNoMountpoint) {
		t.Fatalf("err = %v, want ErrNoMountpoint", err)
	}
}

// TestOpenBrowsableNeedsNoMountpoint is the inversion at the Open level: with no
// mountpoint given, the cartridge still opens and reports where macOS actually
// put it — including through a collision suffix.
func TestOpenBrowsableNeedsNoMountpoint(t *testing.T) {
	tmp := t.TempDir()
	mounted := filepath.Join(tmp, "Volumes", "bladerunner-demo 1")
	openFixture(t, mounted)
	f := &fakeRunner{results: []fakeResult{{stdout: browsablePlist(mounted, openTestDevNode)}}}

	o, err := open(context.Background(), f, filepath.Join(tmp, "demo"+SparseExt), OpenOptions{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { o.releaseClaim() })
	if o.Mountpoint() != resolvePath(mounted) {
		t.Errorf("Mountpoint = %q, want the plist's %q", o.Mountpoint(), resolvePath(mounted))
	}
	if o.Layout.Mountpoint != resolvePath(mounted) {
		t.Errorf("Layout.Mountpoint = %q, want the real mountpoint", o.Layout.Mountpoint)
	}
	if !o.Browsable() {
		t.Error("a default Open must produce a browsable (Finder-ejectable) mount")
	}
	if o.Mount.DevNode != openTestDevNode {
		t.Errorf("Mount.DevNode = %q, want %q", o.Mount.DevNode, openTestDevNode)
	}
	if len(f.calls) != 1 {
		t.Fatalf("hdiutil calls = %v, want a single attach", f.calls)
	}
	for _, arg := range f.calls[0] {
		if arg == flagMountpoint || arg == flagNoBrowse {
			t.Fatalf("default Open dictated the mount: %v", f.calls[0])
		}
	}
}

// TestOpenBrowsableUnwindsARejectedCartridge keeps the browsable path honest
// about the invariant the private one already had: a volume we refuse to boot
// is never left mounted — and now it would be left mounted somewhere VISIBLE.
func TestOpenBrowsableUnwindsARejectedCartridge(t *testing.T) {
	tmp := t.TempDir()
	mounted := filepath.Join(tmp, "Volumes", "bladerunner-demo")
	if err := os.MkdirAll(mounted, layoutDirPerm); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f := &fakeRunner{results: []fakeResult{{stdout: browsablePlist(mounted, openTestDevNode)}}}

	_, err := open(context.Background(), f, filepath.Join(tmp, "demo"+SparseExt), OpenOptions{})
	if !errors.Is(err, ErrNotCartridge) {
		t.Fatalf("err = %v, want ErrNotCartridge", err)
	}
	if len(f.calls) != 2 || f.calls[1][1] != cmdDetach || f.calls[1][2] != resolvePath(mounted) {
		t.Fatalf("hdiutil calls = %v, want a detach of the real mountpoint", f.calls)
	}
}

func TestOpenRejectsAnUnknownPolicy(t *testing.T) {
	f := &fakeRunner{}
	_, err := open(context.Background(), f, filepath.Join(t.TempDir(), "demo"+SparseExt), OpenOptions{Policy: "sideways"})
	if err == nil || !strings.Contains(err.Error(), "sideways") {
		t.Fatalf("err = %v, want an unknown-policy error", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("an invalid policy must not reach hdiutil: %v", f.calls)
	}
}

// TestOpenRejectsANonCartridgeAndUnwinds is the reason Open verifies: attaching
// an arbitrary image must not leave it mounted once we know it is not bootable.
func TestOpenRejectsANonCartridgeAndUnwinds(t *testing.T) {
	tmp := t.TempDir()
	mp := filepath.Join(tmp, "mnt", "demo")
	if err := os.MkdirAll(mp, layoutDirPerm); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f := &fakeRunner{results: []fakeResult{attachResult(mp)}}

	_, err := open(context.Background(), f, filepath.Join(tmp, "demo"+SparseExt), privateOpen(mp))
	if !errors.Is(err, ErrNotCartridge) {
		t.Fatalf("err = %v, want ErrNotCartridge", err)
	}
	if len(f.calls) != 2 || f.calls[1][1] != cmdDetach {
		t.Fatalf("hdiutil calls = %v, want the attach to be unwound by a detach", f.calls)
	}
}

func TestOpenRejectsAFutureFormatAndUnwinds(t *testing.T) {
	tmp := t.TempDir()
	mp := filepath.Join(tmp, "mnt", "demo")
	openFixture(t, mp)
	writeFormatVersion(t, mp, FormatVersion+1)
	f := &fakeRunner{results: []fakeResult{attachResult(mp)}}

	_, err := open(context.Background(), f, filepath.Join(tmp, "demo"+SparseExt), privateOpen(mp))
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
		{stdout: emptyInfoPlist}, // nothing attached: the stale copy is clearable
		{},
		{stderr: "hdiutil: attach failed", err: errors.New("exit 1")},
	}}

	_, err := open(context.Background(), f, dmg, privateOpen(filepath.Join(tmp, "mnt")))
	if err == nil {
		t.Fatal("expected an attach error")
	}
	if _, statErr := os.Stat(work); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("working copy %s should have been removed: %v", work, statErr)
	}
	// The failed open released its claim, so the cartridge is bootable again.
	if holder, busy := Busy(dmg); busy {
		t.Fatalf("a failed open left the cartridge claimed by %s", holder)
	}
}

// TestOpenRefusesACartridgeAnotherProcessBooted is the data-loss regression.
//
// A running VM's disk IS the working copy, and materialize unlinks a stale
// working copy before converting a fresh one over it. With the first boot
// spelled as the .sparseimage and the second as the .dmg it came from, nothing
// derived from a name or a mountpoint connects the two — so before the claim
// existed the second boot silently deleted the first VM's live disk, discarding
// every byte the guest had written.
func TestOpenRefusesACartridgeAnotherProcessBooted(t *testing.T) {
	tmp := t.TempDir()
	mp := filepath.Join(tmp, "mnt", "demo")
	openFixture(t, mp)
	dmg := filepath.Join(tmp, "demo"+DMGExt)
	work := filepath.Join(tmp, "demo"+SparseExt)
	writeFixtureFile(t, work, "live-guest-bytes")

	// First boot: `br boot demo.sparseimage`, still running.
	first := &fakeRunner{results: []fakeResult{attachResult(mp)}}
	running, err := open(context.Background(), first, work, privateOpen(mp))
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	t.Cleanup(func() { running.releaseClaim() })

	// Second boot: `br boot demo.dmg` — the same working copy under another name.
	second := &fakeRunner{results: []fakeResult{{}, attachResult(mp)}}
	if _, err := open(context.Background(), second, dmg, privateOpen(mp)); !errors.Is(err, ErrCartridgeBusy) {
		t.Fatalf("second open = %v, want ErrCartridgeBusy", err)
	}
	if len(second.calls) != 0 {
		t.Fatalf("a refused boot must not run hdiutil: %v", second.calls)
	}
	data, err := os.ReadFile(work)
	if err != nil || string(data) != "live-guest-bytes" {
		t.Fatalf("the running VM's disk was destroyed: %q, %v", data, err)
	}
}

// TestOpenRefusesAWorkingCopyLeftAttachedByADeadHolder is the other half of the
// data-loss regression above, and the half the flock claim CANNOT cover.
//
// flock is a kernel lock: when a holder is SIGKILLed (or force-terminated by
// `br stop --force`, which only removes the control socket) the kernel drops the
// claim, but nothing detaches the cartridge volume or removes the working copy.
// The next boot therefore acquires the now-free claim and used to unconditionally
// unlink the working copy — the backing store of a volume the kernel is still
// serving — which is the same destruction the claim was added to prevent.
func TestOpenRefusesAWorkingCopyLeftAttachedByADeadHolder(t *testing.T) {
	tmp := t.TempDir()
	mp := filepath.Join(tmp, "mnt", "demo")
	openFixture(t, mp)
	dmg := filepath.Join(tmp, "demo"+DMGExt)
	work := filepath.Join(tmp, "demo"+SparseExt)
	writeFixtureFile(t, work, "live-guest-bytes")

	// The dead holder's volume: still attached, claimed by nobody.
	const orphanMount = "/Volumes/bladerunner-demo"
	f := &fakeRunner{results: []fakeResult{attachedImageResult(work, openTestDevNode, orphanMount)}}

	_, err := open(context.Background(), f, dmg, privateOpen(mp))
	if !errors.Is(err, ErrWorkingCopyAttached) {
		t.Fatalf("open = %v, want ErrWorkingCopyAttached", err)
	}
	// A refusal the user cannot act on is its own bug: name the volume and the
	// way to release it.
	for _, want := range []string{orphanMount, openTestDevNode, "hdiutil detach"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	data, readErr := os.ReadFile(work)
	if readErr != nil || string(data) != "live-guest-bytes" {
		t.Fatalf("the orphaned VM's disk was destroyed: %q, %v", data, readErr)
	}
	// Only the attachment probe ran: no convert, no attach.
	if len(f.calls) != 1 || f.calls[0][1] != cmdInfo {
		t.Fatalf("hdiutil calls = %v, want a single info probe", f.calls)
	}
	// And the refusal released the claim, so fixing the mount and retrying works.
	if holder, busy := Busy(dmg); busy {
		t.Fatalf("a refused open left the cartridge claimed by %s", holder)
	}
}

// The guard must not break the case it guards: a working copy left behind by a
// boot that crashed AFTER its volume was detached is stale, not live, and
// re-booting the .dmg still has to clear it (hdiutil convert refuses to
// overwrite).
func TestOpenClearsADetachedStaleWorkingCopy(t *testing.T) {
	tmp := t.TempDir()
	mp := filepath.Join(tmp, "mnt", "demo")
	openFixture(t, mp)
	dmg := filepath.Join(tmp, "demo"+DMGExt)
	work := filepath.Join(tmp, "demo"+SparseExt)
	writeFixtureFile(t, work, "stale-bytes")

	f := &fakeRunner{results: []fakeResult{
		attachedImageResult("/private/tmp/someone-elses.dmg", "/dev/disk4s1", "/Volumes/other"),
		{}, // convert
		attachResult(mp),
	}}

	o, err := open(context.Background(), f, dmg, privateOpen(mp))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { o.releaseClaim() })
	if len(f.calls) != 3 || f.calls[0][1] != cmdInfo || f.calls[1][1] != cmdConvert {
		t.Fatalf("hdiutil calls = %v, want info then convert then attach", f.calls)
	}
}

// The refusal has to name the conflict: "it is busy" with no owner leaves the
// user with nothing to act on.
func TestOpenBusyErrorNamesTheHolder(t *testing.T) {
	tmp := t.TempDir()
	mp := filepath.Join(tmp, "mnt", "demo")
	openFixture(t, mp)
	image := filepath.Join(tmp, "demo"+SparseExt)

	running, err := open(context.Background(), &fakeRunner{results: []fakeResult{attachResult(mp)}}, image, privateOpen(mp))
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	t.Cleanup(func() { running.releaseClaim() })

	_, err = open(context.Background(), &fakeRunner{}, image, privateOpen(mp))
	if err == nil {
		t.Fatal("expected the second open to be refused")
	}
	for _, want := range []string{"demo", strconv.Itoa(os.Getpid())} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}

	holder, busy := Busy(image)
	if !busy || holder.PID != os.Getpid() || holder.Name != "demo" {
		t.Fatalf("Busy() = %+v, %v; want the running holder", holder, busy)
	}
}

// Closing releases the claim, so the same cartridge boots again afterwards —
// the AirDrop -> boot -> eject -> boot cycle.
func TestOpenedCloseReleasesTheClaim(t *testing.T) {
	tmp := t.TempDir()
	mp := filepath.Join(tmp, "mnt", "demo")
	openFixture(t, mp)
	image := filepath.Join(tmp, "demo"+SparseExt)

	o, err := open(context.Background(), &fakeRunner{results: []fakeResult{attachResult(mp)}}, image, privateOpen(mp))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, busy := Busy(image); !busy {
		t.Fatal("an open cartridge must read as busy")
	}
	if err := o.closeWith(context.Background(), &fakeRunner{}); err != nil {
		t.Fatalf("close: %v", err)
	}
	if holder, busy := Busy(image); busy {
		t.Fatalf("close left the cartridge claimed by %s", holder)
	}

	// And a fresh boot of it succeeds.
	openFixture(t, mp)
	again, err := open(context.Background(), &fakeRunner{results: []fakeResult{attachResult(mp)}}, image, privateOpen(mp))
	if err != nil {
		t.Fatalf("re-open after close: %v", err)
	}
	again.releaseClaim()
}

// Busy is a probe, so it must never create the lock file it looks for: a
// cartridge nobody has booted is not busy and gains no state from being asked.
func TestBusyIsSideEffectFree(t *testing.T) {
	tmp := t.TempDir()
	image := filepath.Join(tmp, "demo"+SparseExt)
	if holder, busy := Busy(image); busy {
		t.Fatalf("an unbooted cartridge reported busy: %+v", holder)
	}
	if _, busy := Busy(""); busy {
		t.Fatal("an empty path cannot be busy")
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("Busy() left files behind: %v", entries)
	}
}

// The claim is keyed on the working copy, which is what makes the two spellings
// of one cartridge the same cartridge.
func TestWorkingCopyPathAndClaimIdentity(t *testing.T) {
	tmp := t.TempDir()
	dmg := filepath.Join(tmp, "demo"+DMGExt)
	sparse := filepath.Join(tmp, "demo"+SparseExt)

	if got := WorkingCopyPath(dmg); got != sparse {
		t.Errorf("WorkingCopyPath(%q) = %q, want %q", dmg, got, sparse)
	}
	if got := WorkingCopyPath(sparse); got != sparse {
		t.Errorf("a runnable image is its own working copy, got %q", got)
	}
	if lockPathFor(WorkingCopyPath(dmg)) != lockPathFor(WorkingCopyPath(sparse)) {
		t.Error("the two spellings of one cartridge must share a claim")
	}
	// The lock file is a hidden sibling, so it never shows up in Finder next to
	// the cartridge the user AirDropped.
	if base := filepath.Base(lockPathFor(sparse)); !strings.HasPrefix(base, ".") {
		t.Errorf("lock file %q is not hidden", base)
	}
}

func TestCanonicalImagePath(t *testing.T) {
	tmp := t.TempDir()
	image := filepath.Join(tmp, "demo"+SparseExt)

	// A path that does not exist still canonicalizes (its directory does).
	if got := CanonicalImagePath(image); got != filepath.Join(resolvePath(tmp), "demo"+SparseExt) {
		t.Errorf("CanonicalImagePath(%q) = %q", image, got)
	}
	// Traversal and trailing separators collapse onto the same key.
	noisy := filepath.Join(tmp, "sub", "..", "demo"+SparseExt)
	if CanonicalImagePath(noisy) != CanonicalImagePath(image) {
		t.Errorf("CanonicalImagePath(%q) != CanonicalImagePath(%q)", noisy, image)
	}
	if CanonicalImagePath("") != "" {
		t.Error("an empty path canonicalizes to nothing")
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
