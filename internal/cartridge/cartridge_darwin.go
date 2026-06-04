//go:build darwin

package cartridge

import (
	"os"
	"path/filepath"
	"syscall"
)

// hostSupported reports whether the cartridge machinery (hdiutil + APFS images)
// is available. It is always true on darwin.
func hostSupported() bool { return true }

// isMountpoint reports whether resolved is the root of a mounted filesystem by
// comparing its device id with its parent's: a mounted volume lives on a
// different device than the directory it is mounted over.
func isMountpoint(resolved string) bool {
	var st syscall.Stat_t
	if err := syscall.Lstat(resolved, &st); err != nil {
		return false
	}
	parent := filepath.Dir(resolved)
	if parent == resolved {
		return true // filesystem root
	}
	var parentSt syscall.Stat_t
	if err := syscall.Lstat(parent, &parentSt); err != nil {
		// If the parent cannot be stat'd, fall back to a plain existence check.
		_, statErr := os.Stat(resolved)
		return statErr == nil
	}
	return st.Dev != parentSt.Dev
}
