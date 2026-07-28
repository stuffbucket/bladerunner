package util

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// One guarantee of WriteFileAtomic is NOT covered below: that the destination's
// parent directory is fsynced after the rename, so the rename survives a host
// crash. That property is only observable across a power failure or a kernel
// crash; no in-process test can see it, because a successful fsync and an
// omitted fsync are indistinguishable to a reader on a live machine. Rather
// than write an assertion that only looks like a test, we state it here: the
// directory fsync in syncDir is verified by review, not by this file.
//
// Every other test in this file is written to FAIL if WriteFileAtomic is
// replaced by a plain os.WriteFile. A test that passes against both
// implementations does not test atomicity.

const (
	// tornPayloadSize is large enough that a non-atomic writer leaves the
	// destination short or empty for an observable interval.
	tornPayloadSize = 256 << 10
	// tornIterations is the number of overwrites the hammering tests perform.
	tornIterations = 60
	// concurrentPayloadSize sizes the distinct payloads of racing writers.
	concurrentPayloadSize = 128 << 10
	// distinctWriters is the number of goroutines that race on one path.
	distinctWriters = 8
	// concurrentRounds is the number of writes each racing goroutine performs.
	concurrentRounds = 8
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

// reportOnce records the first violation a hammering reader finds and lets the
// reader stop. The channel is buffered, so the send never blocks.
func reportOnce(ch chan<- string, msg string) {
	select {
	case ch <- msg:
	default:
	}
}

// hammer runs read against the destination in a tight loop until stop closes.
// It returns the first violation read reported, or the empty string.
func hammer(read func(chan<- string) bool) func() string {
	violations := make(chan string, 1)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if !read(violations) {
				return
			}
		}
	}()
	return func() string {
		close(stop)
		<-done
		select {
		case msg := <-violations:
			return msg
		default:
			return ""
		}
	}
}

// alternate overwrites path with a and b in turn. Both payloads have the same
// length, so any other length a reader sees is a torn or truncated file.
func alternate(t *testing.T, path string, a, b []byte) {
	t.Helper()
	for i := range tornIterations {
		payload := a
		if i%2 == 1 {
			payload = b
		}
		if err := WriteFileAtomic(path, payload, 0o600); err != nil {
			t.Fatalf("WriteFileAtomic (iteration %d): %v", i, err)
		}
	}
}

// A reader that opens the destination at an arbitrary instant must observe one
// complete payload. A plain os.WriteFile truncates and then refills the
// destination in place, so the same reader observes a prefix.
func TestWriteFileAtomicReaderNeverSeesPartialContents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hammered.json")
	before := bytes.Repeat([]byte("O"), tornPayloadSize)
	after := bytes.Repeat([]byte("N"), tornPayloadSize)

	if err := WriteFileAtomic(path, before, 0o600); err != nil {
		t.Fatalf("WriteFileAtomic (seed): %v", err)
	}

	finish := hammer(func(violations chan<- string) bool {
		got, err := os.ReadFile(path)
		if err != nil {
			reportOnce(violations, fmt.Sprintf("destination unreadable mid-write: %v", err))
			return false
		}
		if bytes.Equal(got, before) || bytes.Equal(got, after) {
			return true
		}
		reportOnce(violations, fmt.Sprintf(
			"observed %d bytes, which is neither the complete previous payload (%d bytes of %q) nor the complete new one (%d bytes of %q)",
			len(got), len(before), "O", len(after), "N"))
		return false
	})

	alternate(t, path, before, after)

	if msg := finish(); msg != "" {
		t.Errorf("reader saw a partially written file: %s", msg)
	}
}

// The destination must never vanish or shrink to zero. A plain os.WriteFile
// opens with O_TRUNC, so the file is briefly empty on every overwrite.
func TestWriteFileAtomicDestinationNeverEmptyOrAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "always-there.json")
	before := bytes.Repeat([]byte("O"), tornPayloadSize)
	after := bytes.Repeat([]byte("N"), tornPayloadSize)

	if err := WriteFileAtomic(path, before, 0o600); err != nil {
		t.Fatalf("WriteFileAtomic (seed): %v", err)
	}

	finish := hammer(func(violations chan<- string) bool {
		info, err := os.Stat(path)
		if err != nil {
			reportOnce(violations, fmt.Sprintf("destination absent mid-write: %v", err))
			return false
		}
		if info.Size() != int64(tornPayloadSize) {
			reportOnce(violations, fmt.Sprintf(
				"destination held %d bytes mid-write, want %d", info.Size(), tornPayloadSize))
			return false
		}
		return true
	})

	alternate(t, path, before, after)

	if msg := finish(); msg != "" {
		t.Errorf("destination was not continuously complete: %s", msg)
	}
}

// An overwrite must apply the mode the caller asks for. A plain os.WriteFile
// keeps the mode the destination already has, because open(2) only applies the
// mode when it creates the file.
func TestWriteFileAtomicOverwriteAppliesRequestedMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mode.json")

	if err := WriteFileAtomic(path, []byte("first"), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic (create): %v", err)
	}
	for _, perm := range []fs.FileMode{0o644, 0o600, 0o640} {
		if err := WriteFileAtomic(path, []byte("again"), perm); err != nil {
			t.Fatalf("WriteFileAtomic (overwrite %o): %v", perm, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if got := info.Mode().Perm(); got != perm {
			t.Errorf("mode after overwrite = %o, want %o", got, perm)
		}
	}
}

// countTemps returns the names in dir whose name marks them as staging files.
func countTemps(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	var temps []string
	for _, de := range des {
		if strings.Contains(de.Name(), ".tmp") {
			temps = append(temps, de.Name())
		}
	}
	return temps
}

// A write that fails at the rename must remove its staging file and leave the
// destination untouched.
func TestWriteFileAtomicFailedWriteLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "occupied")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	child := filepath.Join(path, "child")
	if err := os.WriteFile(child, []byte("keep me"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// The rename cannot replace a directory, so the publish step fails.
	if err := WriteFileAtomic(path, []byte("x"), 0o600); err == nil {
		t.Fatalf("WriteFileAtomic onto a directory: err = nil, want an error")
	}

	if temps := countTemps(t, dir); len(temps) != 0 {
		t.Errorf("failed write left staging files behind: %v", temps)
	}
	if got, err := os.ReadFile(child); err != nil || string(got) != "keep me" {
		t.Errorf("failed write disturbed the destination: contents = %q, err = %v", got, err)
	}
}

// A write into a directory that does not exist must fail without leaving a
// staging file in the parent.
func TestWriteFileAtomicMissingDirectoryLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "no-such-dir", "record.json")

	if err := WriteFileAtomic(path, []byte("x"), 0o600); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("WriteFileAtomic into a missing directory: err = %v, want fs.ErrNotExist", err)
	}

	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(des) != 0 {
		t.Errorf("failed write left %d entries in the parent: %v", len(des), des)
	}
}

// Writers that race on one path with DIFFERENT payloads must leave exactly one
// complete payload, and a reader watching the race must never see anything but
// a complete payload. A plain os.WriteFile survives the first half of this
// check, because POSIX serializes concurrent writes to one regular file, but it
// fails the second: each writer truncates the destination before it refills it.
func TestWriteFileAtomicConcurrentDistinctPayloads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "raced.json")

	payloads := make([][]byte, distinctWriters)
	for i := range payloads {
		payloads[i] = bytes.Repeat([]byte{byte('a' + i)}, concurrentPayloadSize)
	}
	complete := func(b []byte) bool {
		for _, payload := range payloads {
			if bytes.Equal(b, payload) {
				return true
			}
		}
		return false
	}

	// Seed the destination so the reader has something complete to find from
	// its first attempt.
	if err := WriteFileAtomic(path, payloads[0], 0o600); err != nil {
		t.Fatalf("WriteFileAtomic (seed): %v", err)
	}

	finish := hammer(func(violations chan<- string) bool {
		got, err := os.ReadFile(path)
		if err != nil {
			reportOnce(violations, fmt.Sprintf("destination unreadable during the race: %v", err))
			return false
		}
		if complete(got) {
			return true
		}
		reportOnce(violations, fmt.Sprintf(
			"observed %d bytes during the race, which is no writer's complete payload (want %d bytes of one repeated character, saw %q...%q)",
			len(got), concurrentPayloadSize, firstRune(got), lastRune(got)))
		return false
	})

	var wg sync.WaitGroup
	errCh := make(chan error, distinctWriters*concurrentRounds)
	for i := range distinctWriters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range concurrentRounds {
				if err := WriteFileAtomic(path, payloads[i], 0o600); err != nil {
					errCh <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent WriteFileAtomic: %v", err)
	}

	if msg := finish(); msg != "" {
		t.Errorf("reader saw a partial file during the race: %s", msg)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !complete(got) {
		t.Errorf("survivor is not any single writer's payload: %d bytes, want %d bytes of one repeated character (saw %q...%q)",
			len(got), concurrentPayloadSize, firstRune(got), lastRune(got))
	}
	if temps := countTemps(t, dir); len(temps) != 0 {
		t.Errorf("racing writers left staging files behind: %v", temps)
	}
}

// firstRune and lastRune describe a payload without printing 128 KiB of it.
func firstRune(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return string(b[0])
}

func lastRune(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return string(b[len(b)-1])
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
