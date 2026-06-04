package main

import (
	"path/filepath"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/config"
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
