//go:build darwin

package cartridge

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// mountReadOnly reports whether the filesystem serving path is mounted
// read-only — true for a shipped UDZO .dmg, false for a runnable .sparseimage.
//
// statfs answers for the CONTAINING mount, so callers must first establish that
// path is a mount root (compare lookupMount's Mountpoint with the query);
// otherwise this reports the enclosing volume's flag, which for a directory on
// the sealed system volume would be a confident and wrong "read-only".
func mountReadOnly(path string) (bool, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return false, fmt.Errorf("statfs %q: %w", path, err)
	}
	return st.Flags&unix.MNT_RDONLY != 0, nil
}
