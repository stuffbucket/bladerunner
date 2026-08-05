package vm_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/vm"
)

// TestMoveSavedState calls the exported name from outside the package, on the
// ordinary same-filesystem path `br save --path` takes when the destination is
// on the same volume: both halves of the generation arrive, the source keeps
// nothing, and the destination still verifies the disk it was stamped against.
func TestMoveSavedState(t *testing.T) {
	dir := t.TempDir()
	diskPath := filepath.Join(dir, "disk.raw")
	if err := os.WriteFile(diskPath, []byte("disk-contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "saved-state.bin")
	if err := os.WriteFile(src, []byte("saved-guest-ram"), 0o600); err != nil {
		t.Fatal(err)
	}
	sidecar := `{"cpus":4,"memory_gib":8,"disk_size_gib":64,"disk_path":"` + diskPath + `"}`
	if err := os.WriteFile(vm.SaveMetadataPath(src), []byte(sidecar), 0o600); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "snapshots", "elsewhere.bin")
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := vm.MoveSavedState(src, dst); err != nil {
		t.Fatalf("MoveSavedState: %v", err)
	}

	b, err := os.ReadFile(dst)
	if err != nil || string(b) != "saved-guest-ram" {
		t.Fatalf("destination state = %q, %v", b, err)
	}
	meta, err := vm.LoadSaveMetadata(dst)
	if err != nil {
		t.Fatalf("destination sidecar: %v", err)
	}
	if meta.DiskPath != diskPath || meta.CPUs != 4 {
		t.Errorf("sidecar not carried across: %+v", meta)
	}
	if _, err := os.Stat(src); err == nil {
		t.Error("source state file survives the move")
	}
	if _, err := os.Stat(vm.SaveMetadataPath(src)); err == nil {
		t.Error("source sidecar survives the move")
	}

	// Moving a path onto itself is a no-op, not a way to lose the generation.
	if err := vm.MoveSavedState(dst, dst); err != nil {
		t.Fatalf("MoveSavedState onto itself: %v", err)
	}
	if _, err := vm.LoadSaveMetadata(dst); err != nil {
		t.Errorf("self-move disturbed the generation: %v", err)
	}
}

// TestMoveSavedStateMissingSource reports a missing source rather than creating
// anything at the destination.
func TestMoveSavedStateMissingSource(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "elsewhere.bin")
	if err := vm.MoveSavedState(filepath.Join(dir, "absent.bin"), dst); err == nil {
		t.Fatal("MoveSavedState reported success for a source that does not exist")
	}
	if _, err := os.Stat(dst); err == nil {
		t.Error("a destination file was created for a missing source")
	}
}
