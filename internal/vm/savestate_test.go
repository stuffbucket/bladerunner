package vm

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveMetadataRoundTripAndVerify(t *testing.T) {
	dir := t.TempDir()
	diskPath := filepath.Join(dir, "disk.raw")
	if err := os.WriteFile(diskPath, []byte("disk-contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	savePath := filepath.Join(dir, "saved-state.bin")

	if err := writeSaveMetadata(savePath, 4, 8, 64, true, diskPath, "bladerunner-share"); err != nil {
		t.Fatalf("writeSaveMetadata: %v", err)
	}

	meta, err := LoadSaveMetadata(savePath)
	if err != nil {
		t.Fatalf("LoadSaveMetadata: %v", err)
	}
	if meta.CPUs != 4 || meta.MemoryGiB != 8 || meta.DiskSizeGiB != 64 || meta.DiskPath != diskPath {
		t.Errorf("metadata mismatch: %+v", meta)
	}
	if meta.GUI == nil || !*meta.GUI {
		t.Errorf("GUI not round-tripped: %+v", meta.GUI)
	}
	if meta.ShareTag != "bladerunner-share" {
		t.Errorf("ShareTag not round-tripped: %q", meta.ShareTag)
	}

	// Unchanged disk verifies cleanly.
	if err := meta.VerifyDisk(); err != nil {
		t.Errorf("VerifyDisk on unchanged disk: %v", err)
	}

	// Changing the disk (size differs) must be detected.
	if err := os.WriteFile(diskPath, []byte("disk-contents-now-longer"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := meta.VerifyDisk(); err == nil {
		t.Error("VerifyDisk should fail after the disk changed")
	}
}

func TestLoadSaveMetadataOldSidecarHasNilGUI(t *testing.T) {
	// A sidecar written before the GUI field (no "gui" key) must decode to a nil
	// pointer so the restore GUI-parity check is skipped rather than misfiring.
	dir := t.TempDir()
	savePath := filepath.Join(dir, "saved-state.bin")
	old := `{"cpus":4,"memory_gib":8,"disk_size_gib":64,"disk_path":"/x/disk.raw"}`
	if err := os.WriteFile(SaveMetadataPath(savePath), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	meta, err := LoadSaveMetadata(savePath)
	if err != nil {
		t.Fatalf("LoadSaveMetadata: %v", err)
	}
	if meta.GUI != nil {
		t.Errorf("expected nil GUI for a pre-field sidecar, got %v", *meta.GUI)
	}
}

func TestSaveMetadataNoShareOmitsTag(t *testing.T) {
	// A save with no share device records an empty tag, which must be omitted
	// from the JSON (omitempty) so an older no-share sidecar and a fresh no-share
	// sidecar are indistinguishable and both pass the no-share restore parity.
	dir := t.TempDir()
	diskPath := filepath.Join(dir, "disk.raw")
	if err := os.WriteFile(diskPath, []byte("d"), 0o600); err != nil {
		t.Fatal(err)
	}
	savePath := filepath.Join(dir, "saved-state.bin")
	if err := writeSaveMetadata(savePath, 4, 8, 64, false, diskPath, ""); err != nil {
		t.Fatalf("writeSaveMetadata: %v", err)
	}
	b, err := os.ReadFile(SaveMetadataPath(savePath))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "share_tag") {
		t.Errorf("empty share tag should be omitted from sidecar JSON, got:\n%s", b)
	}
	meta, err := LoadSaveMetadata(savePath)
	if err != nil {
		t.Fatalf("LoadSaveMetadata: %v", err)
	}
	if meta.ShareTag != "" {
		t.Errorf("expected empty ShareTag, got %q", meta.ShareTag)
	}
}

func TestLoadSaveMetadataMissing(t *testing.T) {
	_, err := LoadSaveMetadata(filepath.Join(t.TempDir(), "nope.bin"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing sidecar should wrap os.ErrNotExist, got %v", err)
	}
}
