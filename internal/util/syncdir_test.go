package util_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/util"
)

// TestSyncDir calls SyncDir from outside the package, which is where its callers
// live: internal/update syncs the parent of the app bundle after each rename of
// its crash-recoverable swap.
//
// The fsync itself is not observable in process — a successful fsync and an
// omitted one look identical to a reader on a live machine — so what this holds
// is the part a caller depends on: SyncDir accepts a real directory, tolerates a
// path that is not one, and never panics or blocks on either.
func TestSyncDir(t *testing.T) {
	dir := t.TempDir()

	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A real directory, a regular file, and a path that does not exist. None of
	// these may stop the caller, because SyncDir reports nothing.
	util.SyncDir(dir)
	util.SyncDir(file)
	util.SyncDir(filepath.Join(dir, "absent"))

	// The directory is untouched by the sync.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	if len(entries) != 1 || entries[0].Name() != "f" {
		t.Fatalf("SyncDir changed the directory: %v", entries)
	}
}
