package logging

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestRotatingFile_RotatesOnSizeThreshold(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "rot.log")

	rf, err := NewRotatingFile(logPath, RotateOptions{
		MaxSize:    1, // 1 MB
		MaxBackups: 2,
		MaxAge:     1,
	})
	if err != nil {
		t.Fatalf("NewRotatingFile: %v", err)
	}

	// Write ~2.5 MB of data to force at least one rotation.
	chunk := bytes.Repeat([]byte("a"), 64*1024) // 64 KiB
	for i := 0; i < 40; i++ {
		if _, err := rf.File().Write(chunk); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	// Lumberjack rotates synchronously inside Write, but the pump
	// goroutine drains the pipe asynchronously. Give it a moment,
	// then Close to flush.
	time.Sleep(100 * time.Millisecond)
	if err := rf.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}

	var found, rotated int
	for _, e := range entries {
		name := e.Name()
		switch {
		case name == "rot.log":
			found++
		case strings.HasPrefix(name, "rot-") && strings.HasSuffix(name, ".log"):
			rotated++
		}
	}

	if found != 1 {
		t.Errorf("expected rot.log to exist exactly once, found %d (entries=%v)", found, entries)
	}
	if rotated == 0 {
		t.Errorf("expected at least one rotated backup, got 0 (entries=%v)", entries)
	}
}

func TestRotatingFile_CloseIdempotent(t *testing.T) {
	dir := t.TempDir()
	rf, err := NewRotatingFile(filepath.Join(dir, "x.log"), RotateOptions{MaxSize: 1})
	if err != nil {
		t.Fatalf("NewRotatingFile: %v", err)
	}
	if _, err := rf.File().WriteString("hi\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := rf.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}
	if err := rf.Close(); err != nil {
		t.Fatalf("close 2: %v", err)
	}
}

func TestRotatingFile_EmptyPath(t *testing.T) {
	if _, err := NewRotatingFile("", RotateOptions{}); err == nil {
		t.Fatalf("expected error for empty path")
	}
}

// RotateIfLarger serves logs written by a DIFFERENT process (a detached child
// holding an inherited descriptor), where there is nobody on this side to
// notice the file growing. It must rotate only what is already oversized.
func TestRotateIfLargerOnlyRotatesAnOversizedFile(t *testing.T) {
	dir := t.TempDir()
	opts := RotateOptions{MaxSize: 1, MaxBackups: 2}

	// Missing file: nothing to rotate, and nothing created.
	missing := filepath.Join(dir, "missing.log")
	if err := RotateIfLarger(missing, opts); err != nil {
		t.Fatalf("RotateIfLarger on a missing file: %v", err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("RotateIfLarger created %s: %v", missing, err)
	}

	// Small file: left exactly as it was.
	small := filepath.Join(dir, "small.log")
	if err := os.WriteFile(small, []byte("still small\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RotateIfLarger(small, opts); err != nil {
		t.Fatalf("RotateIfLarger on a small file: %v", err)
	}
	data, err := os.ReadFile(small)
	if err != nil || string(data) != "still small\n" {
		t.Fatalf("small log = %q, %v; want it untouched", data, err)
	}

	// Oversized file: rotated away, leaving an empty current file.
	big := filepath.Join(dir, "big.log")
	if err := os.WriteFile(big, make([]byte, bytesPerMB+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RotateIfLarger(big, opts); err != nil {
		t.Fatalf("RotateIfLarger on an oversized file: %v", err)
	}
	info, err := os.Stat(big)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("rotated log = %d bytes, want 0", info.Size())
	}
}

// The caller of RotateIfLarger is a spawner that exits seconds later, so a
// rotation that finishes on a background goroutine does not finish at all.
// Compression must be complete, and the file it replaces must be gone, by the
// time the call returns.
func TestRotateIfLargerCompressesBeforeItReturns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vmd.log")
	content := bytes.Repeat([]byte("x"), bytesPerMB+1)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := RotateIfLarger(path, RotateOptions{MaxSize: 1, MaxBackups: 3, Compress: true}); err != nil {
		t.Fatalf("RotateIfLarger: %v", err)
	}

	backups := backupNames(t, dir, "vmd.log")
	if len(backups) != 1 {
		t.Fatalf("rotated generations = %v, want exactly one", backups)
	}
	// The name is lumberjack's, so a log rotated by either side keeps one
	// series of generations.
	if !lumberjackBackup.MatchString(backups[0]) {
		t.Errorf("rotated generation %q does not carry the backup naming", backups[0])
	}
	if !strings.HasSuffix(backups[0], compressSuffix) {
		t.Fatalf("rotated generation %q is not compressed", backups[0])
	}
	if got := gunzip(t, filepath.Join(dir, backups[0])); !bytes.Equal(got, content) {
		t.Errorf("compressed generation holds %d bytes, want the original %d", len(got), len(content))
	}
}

// MaxBackups is what stops the rotation moving the unbounded file one level up,
// into an unbounded set of generations. It too must be applied inline.
func TestRotateIfLargerAppliesMaxBackupsBeforeItReturns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vmd.log")
	now := time.Now()
	older := backupName(path, now.Add(-2*24*time.Hour))
	newer := backupName(path, now.Add(-1*24*time.Hour))
	stage(t, older, "two days old")
	stage(t, newer, "one day old")
	stage(t, path, strings.Repeat("x", bytesPerMB+1))

	if err := RotateIfLarger(path, RotateOptions{MaxSize: 1, MaxBackups: 2}); err != nil {
		t.Fatalf("RotateIfLarger: %v", err)
	}

	// Three generations exist and two are kept: the one just made and the
	// newest of the two staged.
	backups := backupNames(t, dir, "vmd.log")
	if len(backups) != 2 {
		t.Fatalf("kept generations = %v, want two", backups)
	}
	if _, err := os.Stat(older); !os.IsNotExist(err) {
		t.Errorf("the oldest generation survived MaxBackups: %v", err)
	}
	if _, err := os.Stat(newer); err != nil {
		t.Errorf("MaxBackups removed a generation it should keep: %v", err)
	}
}

// MaxAge is applied in the same call, against the timestamp in the name.
func TestRotateIfLargerAppliesMaxAgeBeforeItReturns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vmd.log")
	now := time.Now()
	expired := backupName(path, now.Add(-10*24*time.Hour))
	fresh := backupName(path, now.Add(-1*24*time.Hour))
	stage(t, expired, "ten days old")
	stage(t, fresh, "one day old")
	stage(t, path, strings.Repeat("x", bytesPerMB+1))

	if err := RotateIfLarger(path, RotateOptions{MaxSize: 1, MaxAge: 3}); err != nil {
		t.Fatalf("RotateIfLarger: %v", err)
	}

	if _, err := os.Stat(expired); !os.IsNotExist(err) {
		t.Errorf("the expired generation survived MaxAge: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("MaxAge removed a generation inside the age limit: %v", err)
	}
}

// The holder logs of every instance share one directory, so retention must act
// on the generations of ONE log and on nothing else.
func TestRotateIfLargerLeavesOtherFilesAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vmd.log")
	other := filepath.Join(dir, "vmd-demo.log")
	otherBackup := backupName(other, time.Now().Add(-30*24*time.Hour))
	notes := filepath.Join(dir, "notes.txt")
	stage(t, other, "another instance's holder log")
	stage(t, otherBackup, "another instance's rotated log")
	stage(t, notes, "not a log at all")
	stage(t, path, strings.Repeat("x", bytesPerMB+1))

	opts := RotateOptions{MaxSize: 1, MaxBackups: 1, MaxAge: 1, Compress: true}
	if err := RotateIfLarger(path, opts); err != nil {
		t.Fatalf("RotateIfLarger: %v", err)
	}

	for _, keep := range []string{other, otherBackup, notes} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("rotation of %s touched %s: %v", path, keep, err)
		}
	}
}

// lumberjackBackup is the shape of a rotated file name: <name>-<UTC
// timestamp><ext>, optionally gzipped.
var lumberjackBackup = regexp.MustCompile(`^vmd-\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}\.\d{3}\.log(\.gz)?$`)

// stage writes one file of the given content for a test to rotate around.
func stage(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("stage %s: %v", path, err)
	}
}

// backupNames returns the names in dir that are rotated generations of the log
// named base.
func backupNames(t *testing.T, dir, base string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	ext := filepath.Ext(base)
	prefix := strings.TrimSuffix(base, ext) + "-"
	var names []string
	for _, e := range entries {
		if _, _, ok := parseBackupName(e.Name(), prefix, ext); ok {
			names = append(names, e.Name())
		}
	}
	return names
}

// gunzip returns the content of a compressed generation.
func gunzip(t *testing.T, path string) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	r, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("read %s as gzip: %v", path, err)
	}
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("decompress %s: %v", path, err)
	}
	return data
}
