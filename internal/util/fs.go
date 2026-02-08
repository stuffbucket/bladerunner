// Package util provides shared filesystem utility functions.
package util

import (
	"os"
	"path/filepath"
	"strings"
)

// FileExists returns true if path exists and is a regular file.
func FileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// DirExists returns true if path exists and is a directory.
func DirExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// SafeJoin safely joins a base directory with a relative path,
// ensuring the result stays within the base directory.
// Returns an error if the path would escape the base directory.
func SafeJoin(base, path string) (string, error) {
	// Clean the path to remove any . or .. components
	cleanPath := filepath.Clean(path)

	// Reject absolute paths
	if filepath.IsAbs(cleanPath) {
		cleanPath = strings.TrimPrefix(cleanPath, string(filepath.Separator))
	}

	// Join with base
	result := filepath.Join(base, cleanPath)

	// Resolve to absolute paths for comparison
	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	absResult, err := filepath.Abs(result)
	if err != nil {
		return "", err
	}

	// Ensure result is within base (has base as prefix)
	if !strings.HasPrefix(absResult, absBase+string(filepath.Separator)) && absResult != absBase {
		return "", &PathEscapeError{Base: base, Path: path}
	}

	return result, nil
}

// PathEscapeError indicates an attempt to escape the base directory.
type PathEscapeError struct {
	Base string
	Path string
}

func (e *PathEscapeError) Error() string {
	return "path escapes base directory: " + e.Path
}
