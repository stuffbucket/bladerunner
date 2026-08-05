package util_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/util"
)

// TestCopyFileDurable calls the exported name from outside the package: the
// copy carries the bytes, takes the mode it was given, and — because it is the
// staging half of a publish — refuses a destination that already exists rather
// than overwriting it.
func TestCopyFileDurable(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source.bin")
	payload := strings.Repeat("saved-guest-ram", 1024)
	if err := os.WriteFile(src, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "staged.bin")
	if err := util.CopyFileDurable(src, dst, 0o600); err != nil {
		t.Fatalf("CopyFileDurable: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read copy: %v", err)
	}
	if string(got) != payload {
		t.Errorf("copy holds %d bytes, want %d", len(got), len(payload))
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("copy mode = %o, want 600", perm)
	}
	// The source is a copy source, not a move source: it stays.
	if _, err := os.Stat(src); err != nil {
		t.Errorf("source disturbed by the copy: %v", err)
	}

	// An existing destination is refused, and keeps its own contents.
	if err := util.CopyFileDurable(src, dst, 0o600); err == nil {
		t.Error("CopyFileDurable overwrote an existing destination")
	}
	if err := util.CopyFileDurable(filepath.Join(dir, "absent.bin"), filepath.Join(dir, "other.bin"), 0o600); err == nil {
		t.Error("CopyFileDurable reported success for a missing source")
	}
}
