//go:build darwin

package cartridge

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// hostSupported reports whether the cartridge machinery (hdiutil + APFS images)
// is available. It is always true on darwin.
func hostSupported() bool { return true }

// isMountpoint reports whether resolved is the root of a mounted filesystem by
// comparing its device id with its parent's: a mounted volume lives on a
// different device than the directory it is mounted over.
//
// This is a cheap pre-filter only — it is true for firmlinks and for any
// unrelated volume as well — so callers that need identity pair it with
// lookupMount.
func isMountpoint(resolved string) bool {
	var st unix.Stat_t
	if err := unix.Lstat(resolved, &st); err != nil {
		return false
	}
	parent := filepath.Dir(resolved)
	if parent == resolved {
		return true // filesystem root
	}
	var parentSt unix.Stat_t
	if err := unix.Lstat(parent, &parentSt); err != nil {
		// If the parent cannot be stat'd, fall back to a plain existence check.
		_, statErr := os.Stat(resolved)
		return statErr == nil
	}
	return st.Dev != parentSt.Dev
}

// lookupMount asks the kernel which filesystem serves resolved. statfs answers
// for the containing mount, so the returned Mountpoint equals the query only
// when the query IS a mount root — which is exactly the discrimination the
// st.Dev comparison cannot make. DevNode is the BSD name DiskArbitration and
// diskutil address the volume by.
func lookupMount(resolved string) (MountInfo, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(resolved, &st); err != nil {
		return MountInfo{}, fmt.Errorf("statfs %q: %w", resolved, err)
	}
	return MountInfo{
		Mountpoint: unix.ByteSliceToString(st.Mntonname[:]),
		DevNode:    unix.ByteSliceToString(st.Mntfromname[:]),
		FSType:     unix.ByteSliceToString(st.Fstypename[:]),
	}, nil
}
