package vm

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// stateContents is the payload standing in for a VZ machine-state file.
const stateContents = "saved-guest-ram"

// linkAcrossFilesystems is a linkFunc that always reports EXDEV, the error
// os.Link and os.Rename both return when source and destination live on
// different filesystems. A test cannot mount a second filesystem, so the
// cross-filesystem branch — the realistic trigger for `br save --path`, which
// advertises an arbitrary destination — is reached by injecting this.
func linkAcrossFilesystems(oldname, newname string) error {
	return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EXDEV}
}

// writeGeneration writes a complete saved-state generation (state file plus
// sidecar) at statePath, stamped against a disk image in the same directory,
// and returns the disk path.
func writeGeneration(t *testing.T, statePath, contents string) string {
	t.Helper()
	diskPath := filepath.Join(filepath.Dir(statePath), "disk.raw")
	if err := os.WriteFile(diskPath, []byte("disk-contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeSaveMetadata(statePath, 6, 12, 96, false, diskPath, ""); err != nil {
		t.Fatalf("writeSaveMetadata: %v", err)
	}
	return diskPath
}

// TestMoveSavedStateAcrossFilesystems is the regression test for issue #223.
// `br save --path` moved the state file with os.Rename and then discarded the
// result of moving the sidecar, so a destination on another filesystem got the
// state file alone (or, when the state rename itself hit EXDEV, nothing at
// all) and was still reported as saved. The move must carry both files and must
// not depend on rename working across a filesystem boundary.
func TestMoveSavedStateAcrossFilesystems(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	src := filepath.Join(srcDir, "saved-state.bin")
	dst := filepath.Join(dstDir, "elsewhere.bin")
	diskPath := writeGeneration(t, src, stateContents)

	if err := moveSavedState(src, dst, linkAcrossFilesystems); err != nil {
		t.Fatalf("moveSavedState across filesystems: %v", err)
	}

	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("destination state file: %v", err)
	}
	if string(b) != stateContents {
		t.Errorf("destination state = %q, want %q", b, stateContents)
	}
	meta, err := LoadSaveMetadata(dst)
	if err != nil {
		t.Fatalf("destination has no usable sidecar: %v", err)
	}
	if meta.DiskPath != diskPath {
		t.Errorf("sidecar disk path = %q, want %q", meta.DiskPath, diskPath)
	}
	// The moved generation must still verify the disk it was stamped against —
	// the whole reason the sidecar has to travel with the state file.
	if err := meta.VerifyDisk(); err != nil {
		t.Errorf("VerifyDisk after the move: %v", err)
	}
	if _, err := os.Stat(src); err == nil {
		t.Error("source state file survives a completed move")
	}
	if _, err := os.Stat(SaveMetadataPath(src)); err == nil {
		t.Error("source sidecar survives a completed move")
	}
	// Staging files must not litter the destination directory.
	entries, err := os.ReadDir(dstDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("destination directory holds %d files, want the state and its sidecar", len(entries))
	}
}

// TestMoveSavedStateRefusesWithoutSidecar proves a state file with no sidecar
// is not transferred at all. Moving it would publish something 'br restore'
// has to refuse, while reporting the destination as saved.
func TestMoveSavedStateRefusesWithoutSidecar(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	src := filepath.Join(srcDir, "saved-state.bin")
	dst := filepath.Join(dstDir, "elsewhere.bin")
	writeGeneration(t, src, stateContents)
	if err := os.Remove(SaveMetadataPath(src)); err != nil {
		t.Fatal(err)
	}

	// An unrelated, complete generation already sits at the destination.
	const existingState = "destination-ram"
	const existingSidecar = `{"cpus":2,"disk_path":"/destination/disk.raw"}`
	if err := os.WriteFile(dst, []byte(existingState), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SaveMetadataPath(dst), []byte(existingSidecar), 0o600); err != nil {
		t.Fatal(err)
	}

	err := moveSavedState(src, dst, os.Link)
	if err == nil {
		t.Fatal("moveSavedState reported success for a state file with no sidecar")
	}
	if !strings.Contains(err.Error(), "sidecar") {
		t.Errorf("refusal does not name the missing sidecar: %v", err)
	}
	if b, readErr := os.ReadFile(dst); readErr != nil || string(b) != existingState {
		t.Errorf("existing destination state was disturbed: %q, %v", b, readErr)
	}
	if b, readErr := os.ReadFile(SaveMetadataPath(dst)); readErr != nil || string(b) != existingSidecar {
		t.Errorf("existing destination sidecar was disturbed: %q, %v", b, readErr)
	}
	if b, readErr := os.ReadFile(src); readErr != nil || string(b) != stateContents {
		t.Errorf("source state was consumed by a refused move: %q, %v", b, readErr)
	}
}

// TestMoveSavedStateDestinationBlockedLeavesEverythingAlone covers a failure
// that lands after staging: the destination sidecar path is occupied by
// something that cannot be replaced. Nothing may be published, and the source
// generation must remain complete and restorable.
func TestMoveSavedStateDestinationBlockedLeavesEverythingAlone(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	src := filepath.Join(srcDir, "saved-state.bin")
	dst := filepath.Join(dstDir, "elsewhere.bin")
	writeGeneration(t, src, stateContents)

	// A non-empty directory where the destination sidecar belongs: it cannot be
	// removed and cannot be renamed over.
	blocked := SaveMetadataPath(dst)
	if err := os.MkdirAll(filepath.Join(blocked, "occupied"), 0o700); err != nil {
		t.Fatal(err)
	}

	err := moveSavedState(src, dst, os.Link)
	if err == nil {
		t.Fatal("moveSavedState reported success although the sidecar could not be published")
	}
	if !strings.Contains(err.Error(), src) {
		t.Errorf("error does not say where the saved state still is: %v", err)
	}
	if _, statErr := os.Stat(dst); statErr == nil {
		t.Error("a state file was published at the destination without its sidecar")
	}
	if _, loadErr := LoadSaveMetadata(src); loadErr != nil {
		t.Errorf("source generation is no longer complete: %v", loadErr)
	}
	if b, readErr := os.ReadFile(src); readErr != nil || string(b) != stateContents {
		t.Errorf("source state was disturbed: %q, %v", b, readErr)
	}
}

// TestPublishMovedGenerationRemovesStateWhenSidecarFails exercises the window
// between the two publishing renames directly: if the sidecar cannot be
// published after the state file has landed, the state file is taken back out,
// so the destination never holds half a generation.
func TestPublishMovedGenerationRemovesStateWhenSidecarFails(t *testing.T) {
	dstDir := t.TempDir()
	dst := filepath.Join(dstDir, "elsewhere.bin")
	stateTmp := filepath.Join(dstDir, "elsewhere.bin.move-staged")
	if err := os.WriteFile(stateTmp, []byte(stateContents), 0o600); err != nil {
		t.Fatal(err)
	}
	missingSidecarTmp := filepath.Join(dstDir, "never-staged")

	if err := publishMovedGeneration(dst, stateTmp, missingSidecarTmp); err == nil {
		t.Fatal("publishMovedGeneration reported success with no sidecar to publish")
	}
	if _, err := os.Stat(dst); err == nil {
		t.Error("the state file remains at the destination without its sidecar")
	}
}

// A destination that NAMES THE SAME FILE by a different spelling must be a
// no-op, not a deletion.
//
// The move stages a link, publishes it over the destination, then removes the
// source generation. When source and destination are the same file, that last
// step unlinks what was just published -- and the move still returns nil, so
// `br save` prints a success for a snapshot that no longer exists.
//
// os.Rename, which this code replaced, was a harmless no-op on an alias, so
// anything weaker than an identity check here is a regression against it. The
// spellings below are not exotic: a trailing-slash path, a relative path from
// inside the state directory, and a symlinked state directory are all ordinary
// (on macOS /tmp and /var are themselves symlinks).
func TestMoveSavedStateTreatsAnAliasAsANoOp(t *testing.T) {
	for _, tc := range []struct {
		name string
		dst  func(dir, src string) string
	}{
		{"double slash", func(dir, _ string) string { return dir + "//saved-state.bin" }},
		{"dot segment", func(dir, _ string) string { return filepath.Join(dir, ".", "saved-state.bin") }},
		{"symlinked parent", func(dir, _ string) string {
			link := filepath.Join(filepath.Dir(dir), "aliased")
			if err := os.Symlink(dir, link); err != nil {
				return "" // symlinks unavailable; the other rows still cover this
			}
			return filepath.Join(link, "saved-state.bin")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "saved-state.bin")
			writeGeneration(t, src, "PRECIOUS")

			dst := tc.dst(dir, src)
			if dst == "" {
				t.Skip("could not construct this alias on this filesystem")
			}
			if err := MoveSavedState(src, dst); err != nil {
				t.Fatalf("MoveSavedState(%s -> alias %s) = %v, want nil", src, dst, err)
			}

			got, err := os.ReadFile(src)
			if err != nil {
				t.Fatalf("the snapshot was destroyed by a move onto its own alias: %v", err)
			}
			if string(got) != "PRECIOUS" {
				t.Errorf("saved state = %q, want it untouched", got)
			}
			if _, err := os.Stat(SaveMetadataPath(src)); err != nil {
				t.Errorf("the sidecar was destroyed by a move onto its own alias: %v", err)
			}
		})
	}
}
