//go:build darwin

package vm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/config"
)

// TestPrepareRestoreRefusesStateWithoutSidecar is the regression test for the
// consumer half of issue #217. prepareRestore used to log "no saved-state
// metadata sidecar; using current config and skipping disk-stamp check" and
// return nil, so the guard that stops saved RAM from being restored against a
// changed disk turned itself off precisely when its input was missing — which
// is what a split generation looks like from the restore side.
func TestPrepareRestoreRefusesStateWithoutSidecar(t *testing.T) {
	dir := t.TempDir()
	savePath := filepath.Join(dir, "saved-state.bin")
	if err := os.WriteFile(savePath, []byte("ram"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := &Runner{cfg: &config.Config{}, restoreFrom: savePath}
	err := r.prepareRestore()
	if err == nil {
		t.Fatal("prepareRestore accepted a saved state with no sidecar, so the disk-stamp check was skipped")
	}
	if !strings.Contains(err.Error(), "sidecar") {
		t.Errorf("refusal does not name the missing sidecar: %v", err)
	}
}

// TestPrepareRestoreVerifiesDisk holds the other end of the same contract: a
// complete generation restores, and the same generation is refused once the
// disk image it was stamped against has changed.
func TestPrepareRestoreVerifiesDisk(t *testing.T) {
	dir := t.TempDir()
	diskPath := filepath.Join(dir, "disk.raw")
	if err := os.WriteFile(diskPath, []byte("disk-contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	savePath := filepath.Join(dir, "saved-state.bin")
	if err := os.WriteFile(savePath, []byte("ram"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeSaveMetadata(savePath, 4, 8, 64, false, diskPath, ""); err != nil {
		t.Fatalf("writeSaveMetadata: %v", err)
	}

	r := &Runner{cfg: &config.Config{}, restoreFrom: savePath}
	if err := r.prepareRestore(); err != nil {
		t.Fatalf("prepareRestore on an unchanged disk: %v", err)
	}
	if r.cfg.CPUs != 4 || r.cfg.MemoryGiB != 8 || r.cfg.DiskSizeGiB != 64 {
		t.Errorf("snapshot hardware config not adopted: %+v", r.cfg)
	}

	if err := os.WriteFile(diskPath, []byte("disk-contents-changed-since"), 0o600); err != nil {
		t.Fatal(err)
	}
	r2 := &Runner{cfg: &config.Config{}, restoreFrom: savePath}
	if err := r2.prepareRestore(); err == nil {
		t.Error("prepareRestore accepted a disk that changed after the save")
	}
}
