package cartridge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// detectCartridgeName is the name every staged cartridge in this file is
// packed under.
const detectCartridgeName = "demo"

// stageCartridge lays out a complete, bootable cartridge at dir — what a real
// hdiutil would have made appear at a mountpoint. Nothing here is mounted: the
// layout half of Detect is plain file I/O, which is exactly why it can be
// tested (and run in Linux CI) without a disk image.
func stageCartridge(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, layoutDirPerm); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := Pack(dir, validSourceManifest(), PackOptions{
		Name:     detectCartridgeName,
		PackedBy: "br-test",
	}); err != nil {
		t.Fatalf("Pack: %v", err)
	}
	writeDetectFile(t, filepath.Join(dir, RootImageFile), "not-really-a-disk")
	return dir
}

// stagedCartridgeVolume stages a cartridge under a volume-named directory, so
// the volume-name derived fields are exercised too.
func stagedCartridgeVolume(t *testing.T) string {
	t.Helper()
	return stageCartridge(t, filepath.Join(t.TempDir(), VolumeName(detectCartridgeName)))
}

func writeDetectFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), layoutFilePerm); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// detectNoHdiutil runs the worker with no command runner, i.e. exactly what
// happens off darwin: the layout verdict without the hdiutil probe.
func detectNoHdiutil(t *testing.T, path string) *Detected {
	t.Helper()
	d, err := detect(context.Background(), nil, path)
	if err != nil {
		t.Fatalf("detect(%q): %v", path, err)
	}
	return d
}

// TestDetectAgreesWithBootOnTheNameOfAPackedCartridge walks the identity of one
// cartridge from the file `br disk pack --out` wrote, through the volume name
// hdiutil bakes in, to the two names the rest of the system reads back: the one
// `br boot <file>` derives (NameFromPath, which becomes the instance name) and
// the one `br watch` reports for the mounted volume (Detected.Name).
//
// They have to be the same string. Before the pack-time fix they were not for
// any cartridge whose output file was not named after its source disk: the
// volume and the on-image metadata carried the DISK's name while the boot
// carried the FILE's, so the name the user was shown was not the name they
// could eject by.
func TestDetectAgreesWithBootOnTheNameOfAPackedCartridge(t *testing.T) {
	// `br disk pack debian-trixie-gui --out smoke-cartridge.sparseimage`.
	image := filepath.Join(t.TempDir(), "smoke-cartridge"+SparseExt)
	name := NameFromPath(image) // exactly what pack and boot both derive

	// The volume hdiutil would create, laid out as pack lays it out.
	mp := filepath.Join(t.TempDir(), VolumeName(name))
	if err := os.MkdirAll(mp, layoutDirPerm); err != nil {
		t.Fatalf("mkdir %s: %v", mp, err)
	}
	if err := Pack(mp, validSourceManifest(), PackOptions{Name: name, PackedBy: "br-test"}); err != nil {
		t.Fatalf("Pack: %v", err)
	}
	writeDetectFile(t, filepath.Join(mp, RootImageFile), "not-really-a-disk")

	d := detectNoHdiutil(t, mp)
	if d.Status != StatusBootable {
		t.Fatalf("Status = %q (%s), want %q", d.Status, d.Reason, StatusBootable)
	}
	for label, got := range map[string]string{
		"Detected.Name (what br watch offers)":  d.Name,
		"NameFromVolume (the mount prefilter)":  NameFromVolume(d.VolumeName),
		"Metadata.Name (the on-image stamp)":    d.Metadata.Name,
		"NameFromPath (what br boot registers)": NameFromPath(image),
	} {
		if got != name {
			t.Errorf("%s = %q, want %q", label, got, name)
		}
	}
}

func TestDetectBootableCartridge(t *testing.T) {
	mp := stagedCartridgeVolume(t)
	d := detectNoHdiutil(t, mp)

	if d.Status != StatusBootable {
		t.Fatalf("Status = %q (%s), want %q", d.Status, d.Reason, StatusBootable)
	}
	if !d.Bootable() || !d.Recognized() {
		t.Errorf("Bootable = %v, Recognized = %v, want both true", d.Bootable(), d.Recognized())
	}
	if d.Name != detectCartridgeName {
		t.Errorf("Name = %q, want %q", d.Name, detectCartridgeName)
	}
	if d.Mountpoint != resolvePath(mp) {
		t.Errorf("Mountpoint = %q, want %q", d.Mountpoint, resolvePath(mp))
	}
	if d.VolumeName != VolumeName(detectCartridgeName) {
		t.Errorf("VolumeName = %q, want %q", d.VolumeName, VolumeName(detectCartridgeName))
	}
	if d.FormatVersion != FormatVersion {
		t.Errorf("FormatVersion = %d, want %d", d.FormatVersion, FormatVersion)
	}
	if d.Manifest == nil {
		t.Fatal("Manifest is nil for a bootable cartridge")
	}
	if d.Manifest.Name != detectCartridgeName {
		t.Errorf("Manifest.Name = %q, want %q", d.Manifest.Name, detectCartridgeName)
	}
	if d.Reason != "" || d.Err != nil {
		t.Errorf("a bootable cartridge carries no reason: %q / %v", d.Reason, d.Err)
	}
}

func TestDetectCartridgeMissingRootImage(t *testing.T) {
	mp := stagedCartridgeVolume(t)
	if err := os.Remove(filepath.Join(mp, RootImageFile)); err != nil {
		t.Fatalf("remove root image: %v", err)
	}

	d := detectNoHdiutil(t, mp)
	if d.Status != StatusUnbootable {
		t.Fatalf("Status = %q, want %q", d.Status, StatusUnbootable)
	}
	if !d.Recognized() || d.Bootable() {
		t.Errorf("a damaged cartridge must be recognized but not bootable: %+v", d)
	}
	if !strings.Contains(d.Reason, RootImageFile) {
		t.Errorf("Reason = %q, want it to name %s", d.Reason, RootImageFile)
	}
	if !errors.Is(d.Err, ErrNotCartridge) {
		t.Errorf("Err = %v, want it to wrap ErrNotCartridge", d.Err)
	}
	// The name still has to come through: the notification says which
	// cartridge is broken, and that is the whole point of the middle status.
	if d.Name != detectCartridgeName {
		t.Errorf("Name = %q, want %q", d.Name, detectCartridgeName)
	}
	if d.Manifest != nil {
		t.Errorf("Manifest = %+v, want nil for an unbootable cartridge", d.Manifest)
	}
}

func TestDetectCartridgeFromTheFuture(t *testing.T) {
	mp := stagedCartridgeVolume(t)
	future := FormatVersion + 1
	if err := WriteMetadata(mp, Metadata{
		FormatVersion: future,
		Name:          detectCartridgeName,
	}); err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}

	d := detectNoHdiutil(t, mp)
	if d.Status != StatusUnbootable {
		t.Fatalf("Status = %q, want %q", d.Status, StatusUnbootable)
	}
	if !errors.Is(d.Err, ErrFormatTooNew) {
		t.Fatalf("Err = %v, want it to wrap ErrFormatTooNew", d.Err)
	}
	if d.FormatVersion != future {
		t.Errorf("FormatVersion = %d, want %d", d.FormatVersion, future)
	}
	if !strings.Contains(d.Reason, "upgrade br") {
		t.Errorf("Reason = %q, want it to tell the user to upgrade", d.Reason)
	}
}

func TestDetectCartridgeWithCorruptManifest(t *testing.T) {
	mp := stagedCartridgeVolume(t)
	writeDetectFile(t, filepath.Join(mp, ManifestFile), "{not json")

	d := detectNoHdiutil(t, mp)
	if d.Status != StatusUnbootable {
		t.Fatalf("Status = %q, want %q", d.Status, StatusUnbootable)
	}
	if d.Err == nil || d.Reason == "" {
		t.Fatalf("a corrupt manifest must be explained: %+v", d)
	}
	// The metadata stamp is still readable, so the cartridge is still named.
	if d.Name != detectCartridgeName {
		t.Errorf("Name = %q, want %q", d.Name, detectCartridgeName)
	}
}

func TestDetectCartridgeWithCorruptMetadata(t *testing.T) {
	mp := stagedCartridgeVolume(t)
	writeDetectFile(t, filepath.Join(mp, MetadataFile), "{not json")

	d := detectNoHdiutil(t, mp)
	if d.Status != StatusUnbootable {
		t.Fatalf("Status = %q, want %q", d.Status, StatusUnbootable)
	}
	// No metadata name survives corruption, so the volume name is the fallback.
	if d.Name != detectCartridgeName {
		t.Errorf("Name = %q, want the volume-derived %q", d.Name, detectCartridgeName)
	}
}

func TestDetectCartridgeWithoutMetadataIsLegacyVersion(t *testing.T) {
	mp := stagedCartridgeVolume(t)
	if err := os.Remove(filepath.Join(mp, MetadataFile)); err != nil {
		t.Fatalf("remove metadata: %v", err)
	}

	d := detectNoHdiutil(t, mp)
	if d.Status != StatusBootable {
		t.Fatalf("Status = %q (%s), want %q", d.Status, d.Reason, StatusBootable)
	}
	if d.FormatVersion != legacyFormatVersion {
		t.Errorf("FormatVersion = %d, want %d", d.FormatVersion, legacyFormatVersion)
	}
	// With no metadata the packed manifest names the cartridge.
	if d.Name != detectCartridgeName {
		t.Errorf("Name = %q, want %q", d.Name, detectCartridgeName)
	}
}

func TestDetectNotACartridge(t *testing.T) {
	root := t.TempDir()

	unrelated := filepath.Join(root, "Time Machine")
	if err := os.MkdirAll(filepath.Join(unrelated, "Backups.backupdb"), layoutDirPerm); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A directory carrying the bladerunner volume name but no manifest is
	// still not a cartridge: the name is a hint, the manifest is the test.
	impostor := filepath.Join(root, VolumeName("impostor"))
	if err := os.MkdirAll(impostor, layoutDirPerm); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	plainFile := filepath.Join(root, "file.dmg")
	writeDetectFile(t, plainFile, "x")

	tests := []struct{ name, path string }{
		{"unrelated volume", unrelated},
		{"bladerunner-named directory with no manifest", impostor},
		{"a file, not a directory", plainFile},
		{"a path that does not exist", filepath.Join(root, "gone")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := detectNoHdiutil(t, tt.path)
			if d.Status != StatusNotCartridge {
				t.Fatalf("Status = %q, want %q", d.Status, StatusNotCartridge)
			}
			if d.Recognized() || d.Bootable() {
				t.Errorf("Recognized = %v, Bootable = %v, want both false", d.Recognized(), d.Bootable())
			}
			if d.Reason == "" {
				t.Error("a negative verdict still needs a loggable reason")
			}
			if d.Name != "" || d.Manifest != nil {
				t.Errorf("a non-cartridge names nothing: %+v", d)
			}
			if d.BootSource() != "" {
				t.Errorf("BootSource = %q, want empty", d.BootSource())
			}
		})
	}
}

func TestDetectRejectsEmptyPath(t *testing.T) {
	d, err := detect(context.Background(), nil, "")
	if !errors.Is(err, ErrNoMountpoint) {
		t.Fatalf("err = %v, want ErrNoMountpoint", err)
	}
	if d != nil {
		t.Fatalf("Detected = %+v, want nil alongside an error", d)
	}
}

func TestDetectRecoversBackingImageAndReadOnly(t *testing.T) {
	mp := stagedCartridgeVolume(t)
	// A read-only .dmg mounted at exactly this path, as hdiutil would report
	// it: the volume is a view, and BootSource must be the file behind it.
	plist := infoPlistFor(resolvePath(mp), "/dev/disk9s1", "/Users/me/Downloads/demo.dmg", false)

	d, err := detect(context.Background(), infoRunner(plist), mp)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if d.Status != StatusBootable {
		t.Fatalf("Status = %q (%s), want %q", d.Status, d.Reason, StatusBootable)
	}
	if d.BackingImage != "/Users/me/Downloads/demo.dmg" {
		t.Errorf("BackingImage = %q, want the shipped dmg", d.BackingImage)
	}
	if d.BootSource() != d.BackingImage {
		t.Errorf("BootSource = %q, want the backing image %q", d.BootSource(), d.BackingImage)
	}
	if !d.ReadOnly {
		t.Error("ReadOnly = false for a volume backed by a non-writable image")
	}
	if d.DevNode == "" {
		t.Error("DevNode is empty; the unmount veto has nothing to register on")
	}
}

func TestDetectWritableWorkingCopy(t *testing.T) {
	mp := stagedCartridgeVolume(t)
	plist := infoPlistFor(resolvePath(mp), "/dev/disk9s1", "/state/images/demo.sparseimage", true)

	d, err := detect(context.Background(), infoRunner(plist), mp)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if d.ReadOnly {
		t.Error("ReadOnly = true for a volume backed by a writable sparse image")
	}
	if d.BackingImage != "/state/images/demo.sparseimage" {
		t.Errorf("BackingImage = %q", d.BackingImage)
	}
}

func TestDetectSurvivesAnUnhelpfulHdiutil(t *testing.T) {
	mp := stagedCartridgeVolume(t)
	f := &fakeRunner{results: []fakeResult{{stderr: "hdiutil: info failed", err: errors.New("exit status 1")}}}

	d, err := detect(context.Background(), f, mp)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	// The layout verdict is what decides bootability; the backing image is
	// additive, so losing it must not downgrade a good cartridge.
	if d.Status != StatusBootable {
		t.Fatalf("Status = %q (%s), want %q", d.Status, d.Reason, StatusBootable)
	}
	if d.BackingImage != "" {
		t.Errorf("BackingImage = %q, want empty", d.BackingImage)
	}
	if d.BootSource() != d.Mountpoint {
		t.Errorf("BootSource = %q, want the mountpoint %q", d.BootSource(), d.Mountpoint)
	}
}

func TestDetectSkipsHdiutilWhenNotACartridge(t *testing.T) {
	dir := t.TempDir()
	f := infoRunner(sampleInfoPlist)
	if _, err := detect(context.Background(), f, dir); err != nil {
		t.Fatalf("detect: %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("hdiutil was run for a non-cartridge: %v", f.calls)
	}
}

func TestDetectedStringIsUsableInANotification(t *testing.T) {
	mp := stagedCartridgeVolume(t)
	bootable := detectNoHdiutil(t, mp)
	if !strings.Contains(bootable.String(), detectCartridgeName) {
		t.Errorf("String() = %q, want it to name the cartridge", bootable.String())
	}

	if err := os.Remove(filepath.Join(mp, RootImageFile)); err != nil {
		t.Fatalf("remove root image: %v", err)
	}
	broken := detectNoHdiutil(t, mp)
	if !strings.Contains(broken.String(), RootImageFile) {
		t.Errorf("String() = %q, want it to say what is missing", broken.String())
	}

	var nilDetected *Detected
	if nilDetected.String() != string(StatusNotCartridge) {
		t.Errorf("nil String() = %q", nilDetected.String())
	}
	if nilDetected.Bootable() || nilDetected.Recognized() || nilDetected.BootSource() != "" {
		t.Error("a nil *Detected must answer negatively, not panic")
	}
}

func TestIsCandidate(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{VolumeName(detectCartridgeName), true},
		{"/Volumes/" + VolumeName(detectCartridgeName), true},
		{"bladerunner-demo 1", true},
		{"bladerunner-a", true},
		{VolumePrefix, false},
		{"", false},
		{"Macintosh HD", false},
		{"/Volumes/Time Machine", false},
		{"Bladerunner-demo", false},
		{"my-bladerunner-demo", false},
	}
	for _, tt := range tests {
		if got := IsCandidate(tt.in); got != tt.want {
			t.Errorf("IsCandidate(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestNameFromVolume(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{VolumeName(detectCartridgeName), detectCartridgeName},
		{"/Volumes/" + VolumeName(detectCartridgeName), detectCartridgeName},
		{"bladerunner-demo 1", detectCartridgeName},
		{"bladerunner-demo 12", detectCartridgeName},
		{"bladerunner-demo v2", "demo v2"},
		{"bladerunner-demo ", "demo "},
		{"bladerunner- 2", " 2"},
		{"unprefixed", "unprefixed"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := NameFromVolume(tt.in); got != tt.want {
			t.Errorf("NameFromVolume(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// infoPlistFor renders an `hdiutil info -plist` document for a single image
// mounted at mountpoint, so tests that must match a t.TempDir() path can still
// drive the real parser. It mirrors attachPlistFor, one level deeper: info
// nests each image's system-entities under an images array.
func infoPlistFor(mountpoint, devNode, imagePath string, writable bool) string {
	writeable := "<false/>"
	if writable {
		writeable = "<true/>"
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>images</key>
	<array>
		<dict>
			<key>image-path</key>
			<string>%s</string>
			<key>image-type</key>
			<string>test image</string>
			<key>removable</key>
			<true/>
			<key>system-entities</key>
			<array>
				<dict>
					<key>content-hint</key>
					<string>GUID_partition_scheme</string>
					<key>dev-entry</key>
					<string>/dev/disk9</string>
				</dict>
				<dict>
					<key>dev-entry</key>
					<string>%s</string>
					<key>mount-point</key>
					<string>%s</string>
					<key>volume-kind</key>
					<string>apfs</string>
				</dict>
			</array>
			<key>writeable</key>
			%s
		</dict>
	</array>
</dict>
</plist>
`, imagePath, devNode, mountpoint, writeable)
}

// TestDetectMountedCartridge_Integration drives the whole chain on real
// hardware: pack a cartridge into a sparse image, ship it as a read-only .dmg,
// attach that .dmg the way a double-click would, and assert Detect recovers the
// verdict, the read-only-ness, and — the part unit tests cannot prove — the
// path of the .dmg FILE behind the mounted view.
//
// Gated behind BLADERUNNER_CARTRIDGE_IT=1 (matching the other hdiutil
// integration test) and skipped in -short, so the default suite never touches
// a real disk image.
func TestDetectMountedCartridge_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping hdiutil integration test in -short mode")
	}
	if os.Getenv("BLADERUNNER_CARTRIDGE_IT") != "1" {
		t.Skip("set BLADERUNNER_CARTRIDGE_IT=1 to run the hdiutil integration test")
	}
	if runtime.GOOS != "darwin" || !hostSupported() {
		t.Skip("cartridges require macOS")
	}
	if _, err := exec.LookPath(hdiutil); err != nil {
		t.Skip("hdiutil not found in PATH")
	}

	dir := t.TempDir()
	imgPath, err := Create(filepath.Join(dir, detectCartridgeName), MinSizeGiB)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(imgPath) })

	// Lay the cartridge out inside the image, then release it.
	mp := filepath.Join(dir, "mnt")
	m, err := Attach(imgPath, mp)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	stageCartridge(t, m.Mountpoint)
	if err := Detach(m.Mountpoint); err != nil {
		t.Fatalf("Detach: %v", err)
	}

	// Ship it, then mount the shipped artifact the way Finder would.
	dmgPath, err := ConvertToDMG(imgPath, filepath.Join(dir, "ship"))
	if err != nil {
		t.Fatalf("ConvertToDMG: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(dmgPath) })

	shipped, err := Attach(dmgPath, filepath.Join(dir, "shipped"))
	if err != nil {
		t.Fatalf("Attach shipped dmg: %v", err)
	}
	t.Cleanup(func() { _ = Detach(shipped.Mountpoint) })

	d, err := Detect(shipped.Mountpoint)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if d.Status != StatusBootable {
		t.Fatalf("Status = %q (%s), want %q", d.Status, d.Reason, StatusBootable)
	}
	if !d.ReadOnly {
		t.Error("ReadOnly = false for a mounted UDZO dmg")
	}
	if d.DevNode != shipped.DevNode {
		t.Errorf("DevNode = %q, want %q", d.DevNode, shipped.DevNode)
	}
	if resolvePath(d.BackingImage) != resolvePath(dmgPath) {
		t.Errorf("BackingImage = %q, want %q", d.BackingImage, dmgPath)
	}
	if d.BootSource() != d.BackingImage {
		t.Errorf("BootSource = %q, want the shipped dmg", d.BootSource())
	}
	if d.Name != detectCartridgeName {
		t.Errorf("Name = %q, want %q", d.Name, detectCartridgeName)
	}
}
