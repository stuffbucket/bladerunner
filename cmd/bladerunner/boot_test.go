package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/cartridge"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/disk"
)

func TestClassifyBootArg(t *testing.T) {
	exists := func(p string) bool { return p == "/tmp/real.disk" }
	tests := []struct {
		name string
		arg  string
		want bootTargetKind
	}{
		{"url", "https://example.com/x-arm64.qcow2", bootTargetURL},
		{"url-no-scheme-dots", "file:///tmp/x.qcow2", bootTargetURL},
		{"existing-disk-file", "/tmp/real.disk", bootTargetFile},
		{"missing-disk-file-falls-to-name", "/tmp/missing.disk", bootTargetName},
		{"plain-name", "incus", bootTargetName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyBootArg(tt.arg, exists)
			if got.kind != tt.want {
				t.Fatalf("classifyBootArg(%q).kind = %v, want %v", tt.arg, got.kind, tt.want)
			}
		})
	}
}

func TestIsCartridgeArg(t *testing.T) {
	tests := []struct {
		arg  string
		want bool
	}{
		{"/tmp/demo.sparseimage", true},
		{"/tmp/demo.dmg", true},
		{"/tmp/real.disk", false},
		{"incus", false},
	}
	for _, tt := range tests {
		if got := isCartridgeArg(tt.arg); got != tt.want {
			t.Errorf("isCartridgeArg(%q) = %v, want %v", tt.arg, got, tt.want)
		}
	}
}

func TestSlotNameFromURL(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://cloud.debian.org/images/debian-13-genericcloud-arm64-20260525-2489.qcow2", "debian-13-genericcloud-arm64-20260525-2489"},
		{"https://example.com/My_Image.IMG", "my-image"},
		{"https://example.com/a.raw", "a"},
		{"---", ""}, // nothing valid survives sanitization
	}
	for _, tt := range tests {
		got := slotNameFromURL(tt.url)
		if got != tt.want {
			t.Fatalf("slotNameFromURL(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

func TestDiskSlotDir(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("BLADERUNNER_STATE_DIR", stateDir)
	want := filepath.Join(stateDir, "disks", "incus")
	if got := diskSlotDir("incus"); got != want {
		t.Fatalf("diskSlotDir = %q, want %q", got, want)
	}
	if got := savedStatePath(diskSlotDir("incus")); got != filepath.Join(want, "saved-state.bin") {
		t.Fatalf("savedStatePath = %q", got)
	}
	_ = config.DefaultStateDir // ensure config import is exercised
}

func TestSizingPrecedence(t *testing.T) {
	// flag > manifest > default.
	if got := pickCPUs(8, 6); got != 8 {
		t.Errorf("flag should win: got %d", got)
	}
	if got := pickCPUs(0, 6); got != 6 {
		t.Errorf("manifest should win when no flag: got %d", got)
	}
	if got := pickCPUs(0, 0); got != config.DefaultCPUs {
		t.Errorf("default should win when neither set: got %d", got)
	}
	if pickDiskGiB(0, 0) != config.DefaultDiskSizeGiB || pickDiskGiB(0, 32) != 32 || pickDiskGiB(16, 32) != 16 {
		t.Fatal("pickDiskGiB precedence wrong")
	}
	if pickMemoryGiB(0, 0) != config.DefaultMemoryGiB || pickMemoryGiB(0, 16) != 16 || pickMemoryGiB(32, 16) != 32 {
		t.Fatal("pickMemoryGiB precedence wrong")
	}
}

// A mistyped cartridge PATH used to be answered with the disk catalog: the
// cartridge branch of classifyBootArg requires the file to exist, so
// "./typo.dmg" fell through to the catalog lookup and the user who had passed a
// path was told to consult a shelf of names ("unknown disk \"./typo.dmg\";
// available disks: ..."). The extension says what was meant, so say that.
func TestResolveBootManifestNamesAMissingCartridgePath(t *testing.T) {
	cat := &disk.Catalog{}
	for _, arg := range []string{"./typo" + cartridge.DMGExt, "/tmp/missing" + cartridge.SparseExt} {
		_, err := resolveBootManifest(bootTarget{kind: bootTargetName, arg: arg}, cat)
		if err == nil {
			t.Fatalf("resolveBootManifest(%q) succeeded, want a not-found error", arg)
		}
		if strings.Contains(err.Error(), "available disks") || strings.Contains(err.Error(), "no disks available") {
			t.Errorf("a missing cartridge path was answered with the disk catalog: %v", err)
		}
		for _, want := range []string{arg, "cartridge", "br disk pack"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	}

	// A plain mistyped NAME still gets the catalog, which is the right answer
	// for it.
	_, err := resolveBootManifest(bootTarget{kind: bootTargetName, arg: "incuss"}, cat)
	if err == nil || !strings.Contains(err.Error(), "disks") {
		t.Errorf("a mistyped catalog name should still be answered with the catalog: %v", err)
	}
}

// `br boot demo.dmg` is the flagship gesture (scripts/smoke-cartridge.sh boots
// a cartridge by path) and appeared nowhere in `br boot --help`, which
// documented resolution as URL / .disk / catalog name only.
func TestBootHelpDocumentsCartridges(t *testing.T) {
	for _, want := range []string{"cartridge", cartridge.SparseExt, cartridge.DMGExt, "br disk pack"} {
		if !strings.Contains(bootCmd.Long, want) {
			t.Errorf("'br boot --help' does not mention %q", want)
		}
	}
}

// 'br watch' called a cartridge "a single .dmg" while 'br disk pack' writes a
// .sparseimage — the inconsistency that steers a user into '--out demo.dmg'.
// Both forms are named wherever a cartridge is described.
func TestWatchHelpNamesBothCartridgeForms(t *testing.T) {
	for _, want := range []string{cartridge.SparseExt, cartridge.DMGExt} {
		if !strings.Contains(watchCmd.Long, want) {
			t.Errorf("'br watch --help' does not mention %q", want)
		}
	}
}
