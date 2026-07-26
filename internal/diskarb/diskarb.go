// Package diskarb is a thin, run-loop-free binding to macOS DiskArbitration.
//
// It exists so bladerunner can (a) notice when a cartridge DMG is mounted or
// unmounted and (b) *veto* an unmount long enough to spin the guest down in an
// orderly fashion. Both are impossible without DiskArbitration: the framework
// is the only supported way to be asked for permission before a volume goes
// away.
//
// # Why a dispatch queue and never a run loop
//
// DiskArbitration offers two ways to receive callbacks:
// DASessionScheduleWithRunLoop and DASessionSetDispatchQueue. This package uses
// the dispatch-queue form exclusively, and callers must not reach for the other
// one.
//
// The reason is the holder process. When bladerunner runs a GUI VM, the
// Virtualization framework's vz.StartGraphicApplication takes over the main
// thread and its CFRunLoop for the entire life of the process and never
// returns. Any DiskArbitration session scheduled on a run loop would therefore
// either be starved (a secondary thread's run loop nobody spins) or would have
// to contend for a run loop this package does not own. A private serial
// dispatch queue (dispatch_queue_create with a NULL attr, i.e.
// DISPATCH_QUEUE_SERIAL) is independent of every run loop in the process, so
// the same code works in the headless holder and under the GUI.
//
// # Callback contract
//
// Every callback registered here is delivered on the session's private serial
// queue — never on the goroutine that registered it, and never on the main
// thread. Two rules follow:
//
//   - Callbacks must return promptly. In particular the unmount-approval
//     callback must not block on a multi-second VM shutdown; its only job is to
//     return a Dissent immediately and kick the drain off asynchronously (a
//     later unmount attempt, or an explicit unmount once the drain finishes,
//     then succeeds).
//   - A callback must not call the CancelFunc of another watcher on the same
//     session from a *different* goroutine and then wait for it, and must not
//     panic: a panic crossing the cgo boundary takes the process down.
//     Canceling from *inside* a callback is explicitly supported.
package diskarb

import (
	"errors"
	"fmt"
	"strings"
)

// ErrUnsupported is returned by every entry point on non-darwin platforms.
// It wraps errors.ErrUnsupported so callers can test with errors.Is.
var ErrUnsupported = fmt.Errorf("diskarb: DiskArbitration is only available on macOS: %w", errors.ErrUnsupported)

// ErrSessionClosed is returned when a Session is used after Close.
var ErrSessionClosed = errors.New("diskarb: session is closed")

// ErrNilCallback is returned when a watcher is registered without a function.
var ErrNilCallback = errors.New("diskarb: callback must not be nil")

// CancelFunc unregisters a watcher. It is idempotent, and it is safe to call it
// after the owning Session has been closed (in which case it does nothing).
//
// When it returns, the watcher's function is guaranteed not to be running and
// guaranteed never to run again — unless it is called from inside that very
// callback, where waiting for the callback to finish would deadlock the
// session's serial queue and the wait is therefore skipped.
type CancelFunc func()

// DiskInfo is a snapshot of the DiskArbitration description of one disk.
//
// Fields are best-effort: DiskArbitration omits keys that do not apply, so a
// disk with no mounted filesystem has an empty VolumePath and a synthesized
// APFS snapshot has no BSDName. Callers should treat "" / false as "not
// reported" rather than as a positive assertion.
type DiskInfo struct {
	// BSDName is the device name without the /dev prefix, e.g. "disk4s1".
	BSDName string
	// VolumeName is the user-visible volume name, e.g. "bladerunner-cartridge".
	VolumeName string
	// VolumePath is the mount point, e.g. "/Volumes/bladerunner-cartridge".
	// Empty when the disk carries no mounted volume.
	VolumePath string
	// VolumeKind is the filesystem type, e.g. "apfs" or "hfs".
	VolumeKind string
	// Ejectable reports whether the media can be ejected (true for a DMG).
	Ejectable bool
	// Removable reports whether the media itself can be removed.
	Removable bool
	// WholeDisk reports whether this is the whole disk rather than a slice.
	WholeDisk bool
	// NetworkVolume reports whether the volume is served over the network.
	NetworkVolume bool
	// DeviceModel is the media model string, e.g. "Disk Image".
	DeviceModel string
}

// Mounted reports whether the disk currently carries a mounted volume.
func (d DiskInfo) Mounted() bool { return d.VolumePath != "" }

// Dissent is the answer to an unmount-approval request. The zero value
// approves; see Approve and Deny.
type Dissent struct {
	// Deny vetoes the unmount. The requester (Finder, diskutil, hdiutil) sees a
	// "volume is in use" style failure and may retry.
	Deny bool
	// Reason is a short human-readable explanation surfaced to the requester.
	// Ignored when Deny is false.
	Reason string
}

// Approve returns a Dissent that allows the unmount to proceed.
func Approve() Dissent { return Dissent{} }

// Deny returns a Dissent that vetoes the unmount with the given reason.
//
// Denying is only ever a delaying tactic: the caller is expected to start an
// orderly VM drain and stop denying (or unmount itself) once the drain is done.
func Deny(reason string) Dissent { return Dissent{Deny: true, Reason: reason} }

// bsdDevicePrefix is the prefix every BSD disk device name carries.
const bsdDevicePrefix = "disk"

// wholeDiskName reduces a BSD device name to its whole-disk form: "disk4s1"
// and "disk4s1s2" both reduce to "disk4". Names that do not look like BSD disk
// devices are returned unchanged.
func wholeDiskName(bsdName string) string {
	if !strings.HasPrefix(bsdName, bsdDevicePrefix) {
		return bsdName
	}
	i := len(bsdDevicePrefix)
	for i < len(bsdName) && bsdName[i] >= '0' && bsdName[i] <= '9' {
		i++
	}
	if i == len(bsdDevicePrefix) {
		return bsdName // "disk" with no unit number: not a real device name
	}
	return bsdName[:i]
}

// bsdNameMatches reports whether a disk with BSD name got should be delivered
// to a watcher that asked for want.
//
// An empty want matches everything. Otherwise the match is on the whole-disk
// unit, so a watcher registered for the whole disk "disk4" also sees its slices
// ("disk4s1"), and vice versa — which is what callers want, because an
// unmount-approval request arrives for the *slice* that holds the filesystem
// while a caller who attached a DMG usually only knows the whole disk.
func bsdNameMatches(want, got string) bool {
	if want == "" {
		return true
	}
	if want == got {
		return true
	}
	if got == "" {
		return false
	}
	return wholeDiskName(want) == wholeDiskName(got)
}
