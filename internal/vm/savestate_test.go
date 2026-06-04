package vm

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveMetadataRoundTripAndVerify(t *testing.T) {
	dir := t.TempDir()
	diskPath := filepath.Join(dir, "disk.raw")
	if err := os.WriteFile(diskPath, []byte("disk-contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	savePath := filepath.Join(dir, "saved-state.bin")

	if err := writeSaveMetadata(savePath, 4, 8, 64, true, diskPath); err != nil {
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

func TestLoadSaveMetadataMissing(t *testing.T) {
	_, err := LoadSaveMetadata(filepath.Join(t.TempDir(), "nope.bin"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing sidecar should wrap os.ErrNotExist, got %v", err)
	}
}
