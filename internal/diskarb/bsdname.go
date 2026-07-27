package diskarb

import "strings"

// BSD device names, and the one place in the tree that knows how to read them.
//
// macOS spells the same device four ways: DiskArbitration reports the bare name
// ("disk4s1"), hdiutil and the instance registry record the block path
// ("/dev/disk4s1"), diskutil will hand back the character path ("/dev/rdisk4s1"),
// and APFS adds a second slice level ("disk4s1s2"). Every consumer needs some
// reduction of those spellings, and each one that reimplemented the reduction
// got a slightly different answer — which is how the unmount veto shipped
// registered under a name DiskArbitration could never match, silently approving
// every eject of a running cartridge.
//
// The rule therefore lives here, once, next to the framework whose vocabulary
// it is: BSDName reduces to what DiskArbitration matches on, WholeDiskName
// reduces to the unit that pairs a whole disk with its slices, and DevPath
// renders the form the on-disk records use. All three accept every spelling and
// all three answer "" for anything that is not a BSD disk device, so a caller
// can use the empty answer as the "not a device" test instead of carrying its
// own predicate.

const (
	// DevDir is the directory the kernel exposes BSD disk devices in.
	DevDir = "/dev/"
	// bsdNamePrefix is the prefix of every BSD disk device name; a real name
	// carries at least one unit digit after it.
	bsdNamePrefix = "disk"
	// rawDevicePrefix marks the character ("raw") device node — "rdisk4s1" is
	// the same device as "disk4s1" and reduces to it.
	rawDevicePrefix = "r"
	// pathSeparator separates the directory of a device reference from its
	// name. A reference that carries one must name DevDir to be a device node.
	pathSeparator = '/'
)

// BSDName reduces any spelling of a BSD disk device to the bare name
// DiskArbitration addresses it by: "/dev/rdisk4s1", "/dev/disk4s1" and
// "disk4s1" all reduce to "disk4s1". Surrounding whitespace is ignored.
//
// It returns "" for anything that is not a BSD disk device — a mountpoint, an
// unrelated device node, the empty string. That matters at the unmount veto: an
// empty watcher filter matches EVERY disk, so a caller must treat "" as "no
// device" and refuse to register rather than pass it on.
func BSDName(ref string) string {
	name := bareDeviceName(ref)
	if unitEnd(name) == 0 {
		return ""
	}
	return name
}

// WholeDiskName reduces any spelling of a BSD disk device to its whole-disk
// unit: "/dev/disk4s1", "disk4s1s2" and "/dev/rdisk4" all reduce to "disk4".
// Anything that is not a BSD disk device reduces to "".
//
// Matching on the unit rather than the exact node is what pairs the two halves
// of a mounted image: a holder records the whole disk it attached ("/dev/disk4")
// while DiskArbitration reports the slice that carries the filesystem
// ("disk4s1"), and an unmount-approval request arrives for the slice.
func WholeDiskName(ref string) string {
	name := bareDeviceName(ref)
	end := unitEnd(name)
	if end == 0 {
		return ""
	}
	return name[:end]
}

// DevPath renders any spelling of a BSD disk device as the /dev path form the
// instance registry and hdiutil record: "disk4s1" and "/dev/rdisk4s1" both
// render as "/dev/disk4s1". Anything that is not a BSD disk device renders as
// "".
func DevPath(ref string) string {
	name := BSDName(ref)
	if name == "" {
		return ""
	}
	return DevDir + name
}

// MatchesFilter reports whether a disk DiskArbitration reported as bsdName
// should be delivered to a watcher registered for filter. It is the rule the
// session applies to every callback, exported because the correctness of the
// unmount veto is exactly "does the filter this Host computed match the name
// DiskArbitration will report", and that is worth asserting from the package
// that computes the filter.
//
// An empty filter matches everything. Otherwise the match is on the whole-disk
// unit, so a watcher registered for the whole disk "disk4" also sees its slices
// ("disk4s1") and vice versa — which is what callers want, because an
// unmount-approval request arrives for the SLICE that holds the filesystem
// while a caller who attached a DMG usually only knows the whole disk. Every
// spelling is accepted on both sides, so a filter given as a device path
// matches the bare name that comes back.
//
// A filter that names no BSD disk at all matches nothing rather than
// everything: "" is the only spelling of "match anything", and a caller that
// accidentally computed one from a mountpoint must not silently arm a watcher
// over every disk on the machine.
func MatchesFilter(filter, bsdName string) bool {
	if filter == "" || filter == bsdName {
		return true
	}
	wantUnit, gotUnit := WholeDiskName(filter), WholeDiskName(bsdName)
	if wantUnit == "" || gotUnit == "" {
		return false
	}
	return wantUnit == gotUnit
}

// bareDeviceName strips the surrounding whitespace, the DevDir directory and
// the raw-device "r" from a device reference, leaving the candidate bare name.
// It makes no claim that the result IS a device name; unitEnd decides that.
//
// A reference that carries a directory other than DevDir is not a device node,
// however its last element is spelled, so it reduces to "". That is what stops
// a MOUNTPOINT from being read as the device its volume is named after:
// "/Volumes/disk9" is a directory in /Volumes, not the disk9 device. Callers
// hand these two kinds of reference to the same function — hdiutil reports a
// dev-entry and a mount point side by side — so the distinction has to be made
// here, or a lookup by mountpoint silently matches an unrelated device.
func bareDeviceName(ref string) string {
	name := strings.TrimSpace(ref)
	if strings.ContainsRune(name, pathSeparator) {
		rest, ok := strings.CutPrefix(name, DevDir)
		if !ok || strings.ContainsRune(rest, pathSeparator) {
			return ""
		}
		name = rest
	}
	return strings.TrimPrefix(name, rawDevicePrefix)
}

// unitEnd returns the index just past the unit number in a bare BSD name
// ("disk4s1" -> 5), or 0 when name is not one. The digit test is what keeps a
// relative path such as "diskimages/x" — or the bare word "disk" — from being
// mistaken for a device.
func unitEnd(name string) int {
	if !strings.HasPrefix(name, bsdNamePrefix) {
		return 0
	}
	i := len(bsdNamePrefix)
	for i < len(name) && name[i] >= '0' && name[i] <= '9' {
		i++
	}
	if i == len(bsdNamePrefix) {
		return 0
	}
	return i
}
