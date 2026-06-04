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

func TestTrimCartridgeExt(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"/tmp/demo.sparseimage", "/tmp/demo"},
		{"/tmp/demo.dmg", "/tmp/demo"},
		{"demo.sparseimage", "demo"},
		{"demo", "demo"}, // no extension, unchanged
	}
	for _, tc := range tests {
		if got := trimCartridgeExt(tc.in); got != tc.want {
			t.Errorf("trimCartridgeExt(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCartridgeNameFromPath(t *testing.T) {
	if got := cartridgeNameFromPath("/some/dir/my-cart.sparseimage"); got != "my-cart" {
		t.Errorf("cartridgeNameFromPath = %q, want my-cart", got)
	}
	if got := cartridgeNameFromPath("/some/dir/shipped.dmg"); got != "shipped" {
		t.Errorf("cartridgeNameFromPath = %q, want shipped", got)
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

func TestCartridgeManifestRewritesImageAndShare(t *testing.T) {
	src := &disk.Manifest{
		Name:  "src",
		Image: disk.ImageSpec{Arches: map[string]disk.ArchImage{"arm64": {URL: "https://x/a.qcow2"}}},
		VM:    disk.VMSpec{DiskSizeGiB: 32},
		Boot:  disk.BootSpec{Mode: disk.BootModeHeadless},
	}
	packed := cartridgeManifest(src, "mycart")

	if packed.Name != "mycart" {
		t.Errorf("packed name = %q, want mycart", packed.Name)
	}
	// Image must point at the local root.img, not a download URL.
	if packed.Image.Path != cartridgeRootImg {
		t.Errorf("packed image path = %q, want %q", packed.Image.Path, cartridgeRootImg)
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

func TestManifestSharePathHelpers(t *testing.T) {
	none := &disk.Manifest{}
	if manifestShareTag(none) != config.DefaultShareTag {
		t.Errorf("default tag = %q", manifestShareTag(none))
	}
	if manifestShareGuestPath(none) != config.DefaultShareGuestPath {
		t.Errorf("default guest path = %q", manifestShareGuestPath(none))
	}

	custom := &disk.Manifest{Share: &disk.ShareSpec{Tag: "mytag", GuestPath: "/data"}}
	if manifestShareTag(custom) != "mytag" {
		t.Errorf("custom tag = %q", manifestShareTag(custom))
	}
	if manifestShareGuestPath(custom) != "/data" {
		t.Errorf("custom guest path = %q", manifestShareGuestPath(custom))
	}
}

func TestApplyBootCartridgeRootsConfigInsideMount(t *testing.T) {
	// Reset the package-global after the test so other tests aren't affected.
	t.Cleanup(func() { bootCartridge.mountpoint = ""; bootCartridge.manifest = nil; bootCartridge.name = "" })

	mp := "/state/mnt/demo"
	bootCartridge.mountpoint = mp
	bootCartridge.manifest = &disk.Manifest{Share: &disk.ShareSpec{Tag: "custom-tag"}}
	bootCartridge.name = "demo"

	cfg := &config.Config{
		BaseImageURL: "https://should-be-cleared",
		ShareDir:     "",
	}
	applyBootCartridge(cfg)

	if cfg.BaseImagePath != filepath.Join(mp, cartridgeRootImg) {
		t.Errorf("BaseImagePath = %q", cfg.BaseImagePath)
	}
	if cfg.DiskPath != filepath.Join(mp, cartridgeRootImg) {
		t.Errorf("DiskPath = %q", cfg.DiskPath)
	}
	if cfg.BaseImageURL != "" {
		t.Errorf("BaseImageURL should be cleared, got %q", cfg.BaseImageURL)
	}
	if cfg.EFIVarsPath != filepath.Join(mp, cartridgeStateDir, cartridgeEFIVarsFile) {
		t.Errorf("EFIVarsPath = %q", cfg.EFIVarsPath)
	}
	if cfg.ShareDir != filepath.Join(mp, cartridgeShareDir) {
		t.Errorf("ShareDir = %q", cfg.ShareDir)
	}
	if cfg.ShareTag != "custom-tag" {
		t.Errorf("ShareTag = %q, want custom-tag", cfg.ShareTag)
	}
}

func TestApplyBootCartridgeNoOpWhenNoCartridge(t *testing.T) {
	t.Cleanup(func() { bootCartridge.mountpoint = "" })
	bootCartridge.mountpoint = ""
	cfg := &config.Config{BaseImageURL: "https://keep", ShareDir: ""}
	applyBootCartridge(cfg)
	if cfg.BaseImageURL != "https://keep" || cfg.ShareDir != "" {
		t.Errorf("applyBootCartridge mutated config for non-cartridge boot: %+v", cfg)
	}
}
