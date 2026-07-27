package cartridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/disk"
)

// validSourceManifest is a plain per-arch disk manifest of the kind `br disk
// pack` starts from.
func validSourceManifest() *disk.Manifest {
	return &disk.Manifest{
		Name:  "src",
		Image: disk.ImageSpec{Arches: map[string]disk.ArchImage{"arm64": {URL: "https://x/a.qcow2"}}},
		VM:    disk.VMSpec{DiskSizeGiB: 32},
		Boot:  disk.BootSpec{Mode: disk.BootModeHeadless},
	}
}

func TestLayoutPaths(t *testing.T) {
	mp := "/state/mnt/demo"
	l := NewLayout(mp)
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"manifest", l.ManifestPath(), filepath.Join(mp, ManifestFile)},
		{"metadata", l.MetadataPath(), filepath.Join(mp, MetadataFile)},
		{"root image", l.RootImagePath(), filepath.Join(mp, RootImageFile)},
		{"state", l.StateDir(), filepath.Join(mp, StateDirName)},
		{"cloud-init", l.CloudInitDir(), filepath.Join(mp, StateDirName, CloudInitDirName)},
		{"efi vars", l.EFIVarsPath(), filepath.Join(mp, StateDirName, EFIVarsFile)},
		{"share", l.ShareDir(), filepath.Join(mp, ShareDirName)},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %q, want %q", tt.name, tt.got, tt.want)
		}
	}
}

func TestLayoutCreateIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	l := NewLayout(dir)
	for range 2 {
		if err := l.Create(); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	for _, d := range l.Dirs() {
		st, err := os.Stat(d)
		if err != nil || !st.IsDir() {
			t.Fatalf("expected directory %s: %v", d, err)
		}
	}
}

func TestLayoutManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	l := NewLayout(dir)
	if err := l.WriteManifest(PackManifest(validSourceManifest(), "mycart")); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	got, err := l.LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if got.Name != "mycart" || got.Image.Path != RootImageFile {
		t.Fatalf("round-tripped manifest = %+v", got)
	}
}

func TestLayoutLoadManifestNamesTheFile(t *testing.T) {
	dir := t.TempDir()
	_, err := NewLayout(dir).LoadManifest()
	if err == nil {
		t.Fatal("expected an error for a missing manifest")
	}
	if want := filepath.Join(dir, ManifestFile); !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q should name %q", err, want)
	}
}

func TestMountpointFor(t *testing.T) {
	if got, want := MountpointFor("/state", "demo"), "/state/mnt/demo"; got != want {
		t.Fatalf("MountpointFor = %q, want %q", got, want)
	}
}

func TestTrimExt(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"/tmp/demo.sparseimage", "/tmp/demo"},
		{"/tmp/demo.dmg", "/tmp/demo"},
		{"demo.sparseimage", "demo"},
		{"demo", "demo"}, // no extension, unchanged
	}
	for _, tc := range tests {
		if got := TrimExt(tc.in); got != tc.want {
			t.Errorf("TrimExt(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNameFromPath(t *testing.T) {
	if got := NameFromPath("/some/dir/my-cart.sparseimage"); got != "my-cart" {
		t.Errorf("NameFromPath = %q, want my-cart", got)
	}
	if got := NameFromPath("/some/dir/shipped.dmg"); got != "shipped" {
		t.Errorf("NameFromPath = %q, want shipped", got)
	}
}

func TestHasImageExt(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"/tmp/demo.sparseimage", true},
		{"/tmp/demo.dmg", true},
		{"/tmp/demo.disk", false},
		{"incus", false},
	}
	for _, tc := range tests {
		if got := HasImageExt(tc.in); got != tc.want {
			t.Errorf("HasImageExt(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestShareHelpersDefaultAndOverride(t *testing.T) {
	none := &disk.Manifest{}
	if ShareTag(none) != config.DefaultShareTag {
		t.Errorf("default tag = %q", ShareTag(none))
	}
	if ShareGuestPath(none) != config.DefaultShareGuestPath {
		t.Errorf("default guest path = %q", ShareGuestPath(none))
	}
	if ShareTag(nil) != config.DefaultShareTag || ShareGuestPath(nil) != config.DefaultShareGuestPath {
		t.Error("a nil manifest must yield the defaults, not panic")
	}

	custom := &disk.Manifest{Share: &disk.ShareSpec{Tag: "mytag", GuestPath: "/data"}}
	if ShareTag(custom) != "mytag" {
		t.Errorf("custom tag = %q", ShareTag(custom))
	}
	if ShareGuestPath(custom) != "/data" {
		t.Errorf("custom guest path = %q", ShareGuestPath(custom))
	}
}

func TestPackManifestRewritesImageAndShare(t *testing.T) {
	src := validSourceManifest()
	packed := PackManifest(src, "mycart")

	if packed.Name != "mycart" {
		t.Errorf("packed name = %q, want mycart", packed.Name)
	}
	// Image must point at the local root.img, not a download URL.
	if packed.Image.Path != RootImageFile {
		t.Errorf("packed image path = %q, want %q", packed.Image.Path, RootImageFile)
	}
	if len(packed.Image.Arches) != 0 || packed.Image.Hosted {
		t.Errorf("packed image should be local-only: %+v", packed.Image)
	}
	// A default RW share is ensured when the source had none.
	if packed.Share == nil || packed.Share.Tag != config.DefaultShareTag || packed.Share.GuestPath != config.DefaultShareGuestPath {
		t.Errorf("packed share = %+v, want default RW share", packed.Share)
	}
	if packed.Share.ReadOnly {
		t.Error("cartridge default share must be read-write")
	}
	// Cloning means the source manifest is untouched.
	if src.Image.Path != "" {
		t.Errorf("source manifest mutated: %+v", src.Image)
	}
	// The packed manifest must be valid.
	if err := packed.Validate(); err != nil {
		t.Fatalf("packed manifest invalid: %v", err)
	}
}

func TestPackManifestKeepsAnExplicitShare(t *testing.T) {
	src := validSourceManifest()
	src.Share = &disk.ShareSpec{Tag: "mytag", GuestPath: "/data", ReadOnly: true}
	packed := PackManifest(src, "mycart")
	if packed.Share.Tag != "mytag" || packed.Share.GuestPath != "/data" || !packed.Share.ReadOnly {
		t.Fatalf("explicit share not preserved: %+v", packed.Share)
	}
}

// TestPackProducesAVerifiableCartridge is the pack-side contract: everything
// Pack writes, plus a root.img the caller materializes, must satisfy Verify.
func TestPackProducesAVerifiableCartridge(t *testing.T) {
	dir := t.TempDir()
	if err := Pack(dir, validSourceManifest(), PackOptions{Name: "mycart", PackedBy: "br-test"}); err != nil {
		t.Fatalf("Pack: %v", err)
	}
	// Pack deliberately leaves root.img to the caller (it needs the image
	// cache), so a packed-but-not-materialized cartridge must NOT verify.
	if IsCartridge(dir) {
		t.Fatal("a cartridge with no root.img must not verify")
	}
	writeFixtureFile(t, filepath.Join(dir, RootImageFile), "not-really-a-disk")

	meta, err := Verify(dir)
	if err != nil {
		t.Fatalf("Verify after Pack: %v", err)
	}
	if meta.FormatVersion != FormatVersion {
		t.Errorf("format version = %d, want %d", meta.FormatVersion, FormatVersion)
	}
	if meta.Name != "mycart" || meta.PackedBy != "br-test" {
		t.Errorf("metadata = %+v", meta)
	}

	// The packed disk.json describes a local, self-contained source.
	raw, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m disk.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if m.Name != "mycart" || m.Image.Path != RootImageFile {
		t.Errorf("packed manifest = %+v", m)
	}
}

func TestListAttachedIgnoresAMissingMountRoot(t *testing.T) {
	if got := ListAttached(t.TempDir()); got != nil {
		t.Fatalf("ListAttached with no mnt dir = %v, want nil", got)
	}
}

// TestListAttachedIgnoresPlainDirectories keeps the identity requirement
// honest: a directory under mnt/ that is not a mounted disk image is not an
// attached cartridge (and off darwin nothing ever is).
func TestListAttachedIgnoresPlainDirectories(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.MkdirAll(MountpointFor(stateDir, "demo"), layoutDirPerm); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if got := ListAttached(stateDir); len(got) != 0 {
		t.Fatalf("ListAttached = %v, want empty", got)
	}
}
