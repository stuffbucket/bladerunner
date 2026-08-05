package update

import (
	"os"
	"path/filepath"
	"testing"
)

// The tests in this file hold the crash-recovery contract of swapBundle. The
// swap moves the installed bundle aside and then moves the staged one into
// place. A crash between those two renames leaves the backup present and the
// destination missing, and that state is a recovery instruction, not litter: the
// next attempt must restore from it before it removes anything.

const (
	bundleName = "Bladerunner.app"
	markerName = "marker"
)

// swapFixture lays out a root with the destination path and the backup path the
// swap uses, and returns both plus a staging directory on the same filesystem.
func swapFixture(t *testing.T) (dst, backup, staging string) {
	t.Helper()
	root := t.TempDir()
	dst = filepath.Join(root, bundleName)
	backup = filepath.Join(root, "."+bundleName+".old")
	var err error
	if staging, err = os.MkdirTemp(root, ".stage-*"); err != nil {
		t.Fatal(err)
	}
	return dst, backup, staging
}

// writeBundle creates a minimal bundle at path whose marker file holds content.
func writeBundle(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(path, "Contents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "Contents", markerName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// readMarker returns the marker content of the bundle at path.
func readMarker(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(path, "Contents", markerName))
	if err != nil {
		t.Fatalf("read marker at %s: %v", path, err)
	}
	return string(b)
}

// TestSwapBundle_RecoversFromInterruptedSwap is the crash-recovery case: only
// the backup survives, and the new install then fails. The old application must
// come back, because the backup is the only complete generation left.
func TestSwapBundle_RecoversFromInterruptedSwap(t *testing.T) {
	dst, backup, staging := swapFixture(t)
	writeBundle(t, backup, "old")

	// The staged bundle is absent, so the install rename fails. This stands in
	// for any failure after recovery.
	missing := filepath.Join(staging, bundleName)

	if err := swapBundle(dst, missing); err == nil {
		t.Fatal("expected swap to fail when the staged bundle is missing")
	}
	if got := readMarker(t, dst); got != "old" {
		t.Fatalf("marker = %q want old: the only complete generation was destroyed", got)
	}
}

// TestSwapBundle_RecoveryThenInstall holds that recovery does not block a good
// install: the backup is restored first, then replaced by the new bundle.
func TestSwapBundle_RecoveryThenInstall(t *testing.T) {
	dst, backup, staging := swapFixture(t)
	writeBundle(t, backup, "old")

	newApp := filepath.Join(staging, bundleName)
	writeBundle(t, newApp, "new")

	if err := swapBundle(dst, newApp); err != nil {
		t.Fatalf("swap: %v", err)
	}
	if got := readMarker(t, dst); got != "new" {
		t.Fatalf("marker = %q want new", got)
	}
	if _, err := os.Lstat(backup); !os.IsNotExist(err) {
		t.Fatalf("backup survived a committed swap: stat err=%v", err)
	}
}

// TestSwapBundle_FailedInstallRestoresPriorApp holds the ordinary failure path:
// the installed application is back where it was and no backup is left behind.
func TestSwapBundle_FailedInstallRestoresPriorApp(t *testing.T) {
	dst, backup, staging := swapFixture(t)
	writeBundle(t, dst, "old")

	missing := filepath.Join(staging, bundleName)
	if err := swapBundle(dst, missing); err == nil {
		t.Fatal("expected swap to fail when the staged bundle is missing")
	}
	if got := readMarker(t, dst); got != "old" {
		t.Fatalf("marker = %q want old", got)
	}
	if _, err := os.Lstat(backup); !os.IsNotExist(err) {
		t.Fatalf("backup left behind after a restored failure: stat err=%v", err)
	}
}

// TestSwapBundle_EveryInterruptionLeavesAGeneration walks the on-disk states the
// swap can be stopped in and holds two things for each: a complete generation is
// present, and a retry converges on the new bundle with no backup left over.
func TestSwapBundle_EveryInterruptionLeavesAGeneration(t *testing.T) {
	cases := []struct {
		name       string
		atDst      string // "" means the path is absent
		atBackup   string
		generation string // the content a launch would find after recovery
	}{
		{name: "before the first rename", atDst: "old", generation: "old"},
		{name: "between the two renames", atBackup: "old", generation: "old"},
		{name: "after the second rename", atDst: "new1", atBackup: "old", generation: "new1"},
		{name: "after cleanup", atDst: "new1", generation: "new1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dst, backup, staging := swapFixture(t)
			if tc.atDst != "" {
				writeBundle(t, dst, tc.atDst)
			}
			if tc.atBackup != "" {
				writeBundle(t, backup, tc.atBackup)
			}

			// A retry that fails at the install step must expose the surviving
			// generation rather than nothing.
			if err := swapBundle(dst, filepath.Join(staging, "absent.app")); err == nil {
				t.Fatal("expected the failing retry to report an error")
			}
			if got := readMarker(t, dst); got != tc.generation {
				t.Fatalf("after a failed retry marker = %q want %q", got, tc.generation)
			}

			// A retry that succeeds must land the new bundle and clean up.
			newApp := filepath.Join(staging, bundleName)
			writeBundle(t, newApp, "new2")
			if err := swapBundle(dst, newApp); err != nil {
				t.Fatalf("retry swap: %v", err)
			}
			if got := readMarker(t, dst); got != "new2" {
				t.Fatalf("marker = %q want new2", got)
			}
			if _, err := os.Lstat(backup); !os.IsNotExist(err) {
				t.Fatalf("backup survived a committed swap: stat err=%v", err)
			}
		})
	}
}

// TestSwapBundle_StaleBackupDroppedWhenDestinationIntact holds the other half of
// the recovery reading: with the destination present the backup is a leftover
// from a committed swap, so removing it is correct.
func TestSwapBundle_StaleBackupDroppedWhenDestinationIntact(t *testing.T) {
	dst, backup, staging := swapFixture(t)
	writeBundle(t, dst, "installed")
	writeBundle(t, backup, "stale")

	newApp := filepath.Join(staging, bundleName)
	writeBundle(t, newApp, "new")

	if err := swapBundle(dst, newApp); err != nil {
		t.Fatalf("swap: %v", err)
	}
	if got := readMarker(t, dst); got != "new" {
		t.Fatalf("marker = %q want new", got)
	}
	if _, err := os.Lstat(backup); !os.IsNotExist(err) {
		t.Fatalf("backup survived a committed swap: stat err=%v", err)
	}
}
