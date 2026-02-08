package util

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileExists(t *testing.T) {
	// Create a temp file
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(tmpFile, []byte("test"), 0o644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	tests := []struct {
		name string
		path string
		want bool
	}{
		{"existing file", tmpFile, true},
		{"non-existent file", filepath.Join(tmpDir, "nonexistent.txt"), false},
		{"directory not a file", tmpDir, false},
		{"empty path", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FileExists(tt.path); got != tt.want {
				t.Errorf("FileExists(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestDirExists(t *testing.T) {
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(tmpFile, []byte("test"), 0o644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	tests := []struct {
		name string
		path string
		want bool
	}{
		{"existing directory", tmpDir, true},
		{"file is not a directory", tmpFile, false},
		{"non-existent path", filepath.Join(tmpDir, "nonexistent"), false},
		{"empty path", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DirExists(tt.path); got != tt.want {
				t.Errorf("DirExists(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestSafeJoin(t *testing.T) {
	tmpDir := t.TempDir()

	tests := []struct {
		name    string
		base    string
		path    string
		want    string
		wantErr bool
	}{
		{
			name:    "simple join",
			base:    tmpDir,
			path:    "subdir/file.txt",
			want:    filepath.Join(tmpDir, "subdir", "file.txt"),
			wantErr: false,
		},
		{
			name:    "path with dots",
			base:    tmpDir,
			path:    "subdir/../other/file.txt",
			want:    filepath.Join(tmpDir, "other", "file.txt"),
			wantErr: false,
		},
		{
			name:    "escape attempt with ..",
			base:    tmpDir,
			path:    "../escape/file.txt",
			want:    "",
			wantErr: true,
		},
		{
			name:    "deep escape attempt",
			base:    tmpDir,
			path:    "subdir/../../escape",
			want:    "",
			wantErr: true,
		},
		{
			name:    "absolute path stripped",
			base:    tmpDir,
			path:    "/absolute/path",
			want:    filepath.Join(tmpDir, "absolute", "path"),
			wantErr: false,
		},
		{
			name:    "current directory",
			base:    tmpDir,
			path:    ".",
			want:    tmpDir,
			wantErr: false,
		},
		{
			name:    "empty path",
			base:    tmpDir,
			path:    "",
			want:    tmpDir,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SafeJoin(tt.base, tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("SafeJoin() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("SafeJoin() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPathEscapeError(t *testing.T) {
	err := &PathEscapeError{Base: "/base", Path: "../escape"}
	msg := err.Error()
	if msg != "path escapes base directory: ../escape" {
		t.Errorf("PathEscapeError.Error() = %q, want %q", msg, "path escapes base directory: ../escape")
	}
}
