package logging

import (
	"bytes"
	"os"
	"path/filepath"
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
