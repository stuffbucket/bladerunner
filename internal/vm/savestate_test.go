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

// TestPublishSaveGenerationSidecarFailureLeavesNothing is the regression test
// for the creation half of the split-generation defect (issue #217). A save
// used to write the state file, log a failure to write the sidecar as a
// warning, and report success — so the new state shipped either with no sidecar
// at all or, worse, with the PREVIOUS save's sidecar still lying beside it,
// stamping a disk the new RAM was never frozen against.
func TestPublishSaveGenerationSidecarFailureLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "saved-state.bin")

	// A previous, complete generation is already on disk.
	if err := os.WriteFile(statePath, []byte("previous-ram"), 0o600); err != nil {
		t.Fatal(err)
	}
	const previousSidecar = `{"cpus":1,"memory_gib":1,"disk_path":"/previous/disk.raw"}`
	if err := os.WriteFile(SaveMetadataPath(statePath), []byte(previousSidecar), 0o600); err != nil {
		t.Fatal(err)
	}

	sidecarErr := errors.New("no space left on device")
	err := publishSaveGeneration(statePath,
		func() error { return os.WriteFile(statePath, []byte("new-ram"), 0o600) },
		func() error { return sidecarErr },
	)
	if err == nil {
		t.Fatal("publishSaveGeneration reported success although the sidecar was never written")
	}
	if !errors.Is(err, sidecarErr) {
		t.Errorf("error does not wrap the sidecar failure: %v", err)
	}
	if b, readErr := os.ReadFile(statePath); readErr == nil {
		t.Errorf("state file survives a failed save and looks restorable: %q", b)
	}
	if b, readErr := os.ReadFile(SaveMetadataPath(statePath)); readErr == nil {
		t.Errorf("a sidecar remains after a failed save: %q", b)
	}
}

// TestPublishSaveGenerationStateFailureLeavesNothing covers the other half: the
// state write fails after the previous generation has been cleared, so neither
// the partial state VZ may have left nor the old sidecar may survive.
func TestPublishSaveGenerationStateFailureLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "saved-state.bin")
	if err := os.WriteFile(SaveMetadataPath(statePath), []byte(`{"cpus":1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	stateErr := errors.New("vz refused the save")
	err := publishSaveGeneration(statePath,
		func() error {
			// VZ can leave a partial file behind when the save fails.
			if writeErr := os.WriteFile(statePath, []byte("partial"), 0o600); writeErr != nil {
				return writeErr
			}
			return stateErr
		},
		func() error { return errors.New("must not be called") },
	)
	if !errors.Is(err, stateErr) {
		t.Fatalf("publishSaveGeneration error = %v, want the state-write failure", err)
	}
	if _, statErr := os.Stat(statePath); statErr == nil {
		t.Error("a partial state file survives a failed save")
	}
	if _, statErr := os.Stat(SaveMetadataPath(statePath)); statErr == nil {
		t.Error("the previous generation's sidecar survives a failed save")
	}
}

// TestPublishSaveGenerationReplacesPreviousGeneration proves the success path
// publishes both halves and keeps nothing of the generation it replaced.
func TestPublishSaveGenerationReplacesPreviousGeneration(t *testing.T) {
	dir := t.TempDir()
	diskPath := filepath.Join(dir, "disk.raw")
	if err := os.WriteFile(diskPath, []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "saved-state.bin")
	if err := os.WriteFile(statePath, []byte("previous-ram"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SaveMetadataPath(statePath), []byte(`{"cpus":1,"disk_path":"/previous/disk.raw"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	err := publishSaveGeneration(statePath,
		func() error { return os.WriteFile(statePath, []byte("new-ram"), 0o600) },
		func() error { return writeSaveMetadata(statePath, 4, 8, 64, false, diskPath, "") },
	)
	if err != nil {
		t.Fatalf("publishSaveGeneration: %v", err)
	}
	b, err := os.ReadFile(statePath)
	if err != nil || string(b) != "new-ram" {
		t.Fatalf("state file = %q, %v; want the new state", b, err)
	}
	meta, err := LoadSaveMetadata(statePath)
	if err != nil {
		t.Fatalf("LoadSaveMetadata: %v", err)
	}
	if meta.DiskPath != diskPath {
		t.Errorf("sidecar describes %q, want the disk of the new save %q", meta.DiskPath, diskPath)
	}
	if err := meta.VerifyDisk(); err != nil {
		t.Errorf("VerifyDisk on the published generation: %v", err)
	}
}

// TestWriteSaveMetadataLeavesNoStagingFile holds the claim that the sidecar
// goes through the atomic-write owner: a staged temp file is renamed into
// place, so nothing that is not the sidecar is left in the directory.
func TestWriteSaveMetadataLeavesNoStagingFile(t *testing.T) {
	dir := t.TempDir()
	diskPath := filepath.Join(dir, "disk.raw")
	if err := os.WriteFile(diskPath, []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "saved-state.bin")
	if err := writeSaveMetadata(statePath, 2, 4, 32, false, diskPath, ""); err != nil {
		t.Fatalf("writeSaveMetadata: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("staging file left behind: %s", e.Name())
		}
	}
}
