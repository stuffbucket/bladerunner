package util

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestWriteFileAtomicCreatesAndOverwrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.json")

	if err := WriteFileAtomic(path, []byte("first\n"), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "first\n" {
		t.Errorf("contents = %q, want %q", got, "first\n")
	}

	if err := WriteFileAtomic(path, []byte("second\n"), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic (overwrite): %v", err)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile after overwrite: %v", err)
	}
	if string(got) != "second\n" {
		t.Errorf("contents = %q, want %q", got, "second\n")
	}
}

func TestWriteFileAtomicHonoursPermAndLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	perms := []fs.FileMode{0o600, 0o644}
	for i, perm := range perms {
		path := filepath.Join(dir, "file"+string(rune('a'+i))+".json")
		if err := WriteFileAtomic(path, []byte("x"), perm); err != nil {
			t.Fatalf("WriteFileAtomic(%o): %v", perm, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if got := info.Mode().Perm(); got != perm {
			t.Errorf("mode = %o, want %o", got, perm)
		}
	}

	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(des) != len(perms) {
		t.Fatalf("directory holds %d files, want %d", len(des), len(perms))
	}
	for _, de := range des {
		if strings.Contains(de.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", de.Name())
		}
	}
}

func TestWriteFileAtomicMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-dir", "record.json")
	err := WriteFileAtomic(path, []byte("x"), 0o600)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("WriteFileAtomic into a missing directory: err = %v, want fs.ErrNotExist", err)
	}
}

// Concurrent writers of the SAME path must never collide on the staging file:
// the random suffix os.CreateTemp substitutes keeps them apart, so every writer
// succeeds and the survivor is one complete payload, never a blend.
func TestWriteFileAtomicConcurrentSamePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "contended.json")
	payload := strings.Repeat("payload", 512)

	const writers = 16
	var wg sync.WaitGroup
	errCh := make(chan error, writers)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := WriteFileAtomic(path, []byte(payload), 0o600); err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent WriteFileAtomic: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != payload {
		t.Errorf("contents are not one complete payload (len %d, want %d)", len(got), len(payload))
	}
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(des) != 1 {
		t.Errorf("directory holds %d files, want 1: %v", len(des), des)
	}
}

func TestPublishFileAtomicReplacesTheTarget(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "cartridge.dmg")
	if err := os.WriteFile(dst, []byte("old"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	src := filepath.Join(dir, ".cartridge.commit.dmg")
	if err := os.WriteFile(src, []byte("new"), 0o644); err != nil {
		t.Fatalf("seed temp: %v", err)
	}

	if err := PublishFileAtomic(src, dst); err != nil {
		t.Fatalf("PublishFileAtomic: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "new" {
		t.Fatalf("dst = %q, %v; want the published content", got, err)
	}
	if _, err := os.Stat(src); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the temporary %s survived the publish: %v", src, err)
	}
}

// A publish that cannot happen must leave the target exactly as it was — that
// is the whole reason the caller builds a temporary first.
func TestPublishFileAtomicLeavesTheTargetAloneOnFailure(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "cartridge.dmg")
	if err := os.WriteFile(dst, []byte("original"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := PublishFileAtomic(filepath.Join(dir, "never-written.dmg"), dst); err == nil {
		t.Fatal("publishing a missing source should fail")
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "original" {
		t.Fatalf("dst = %q, %v; want the original untouched", got, err)
	}
}
