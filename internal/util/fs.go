// Package util provides shared utility functions.
package util

import "os"

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
