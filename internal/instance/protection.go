package instance

// Protection is the recorded outcome of an instance's unmount veto: whether
// the holder armed the DiskArbitration approval callback that turns "eject the
// cartridge in Finder" into an orderly guest shutdown, and if not, why not.
//
// The veto fails OPEN on purpose — a safety net that cannot be registered must
// never stop the VM from starting — so a cartridge can run perfectly well with
// no protection at all. That is exactly why the outcome is recorded here
// rather than merely logged: the holder is a different process from the CLI,
// and a Warn line in its log is not something a user can see. A value here is
// what lets `br instances` and `br status` say "this cartridge is protected" or
// "this cartridge is NOT protected, and here is the reason".
//
// This package owns the vocabulary because the value is written to the
// registry record on disk (Entry.UnmountProtection), so the constants below
// are an on-disk format: a later version reads what an earlier one wrote.
// Never change an existing string; add a new one.
type Protection string

const (
	// ProtectionUnrecorded is the zero value: nothing recorded the outcome.
	// It is what an entry written before this field existed decodes to, and
	// what a non-cartridge instance carries, so it must never be rendered as
	// "protected" — the honest reading is "not known".
	ProtectionUnrecorded Protection = ""
	// ProtectionArmed means the veto is registered: an eject is held until the
	// guest has shut down and the VM has released the mount.
	ProtectionArmed Protection = "armed"
	// ProtectionNoCartridge means the cartridge step produced no open mount on
	// an instance that expected one, so there was no device to watch.
	ProtectionNoCartridge Protection = "no-cartridge"
	// ProtectionNoDevNode means the mount recorded no device node.
	ProtectionNoDevNode Protection = "no-device-node"
	// ProtectionUnreadableDevNode means the recorded device node does not name
	// a BSD disk, so no watch filter could be derived from it. Registering the
	// empty filter instead would arm the veto over every disk on the machine.
	ProtectionUnreadableDevNode Protection = "unreadable-device-node"
	// ProtectionNoSession means the DiskArbitration session could not be
	// opened.
	ProtectionNoSession Protection = "diskarbitration-unavailable"
	// ProtectionWatchFailed means the session opened but the approval watcher
	// could not be registered on it.
	ProtectionWatchFailed Protection = "watch-registration-failed"
	// ProtectionUnsupported means the host has no DiskArbitration at all.
	// It is a statement about the platform, not a degradation of this install
	// — see Degraded.
	ProtectionUnsupported Protection = "unsupported-platform"
)

// unrecordedCode is the code reported for the zero value. The on-disk spelling
// is the empty string so the field is omitted from a record entirely, but a
// JSON consumer needs a name to switch on rather than "".
const unrecordedCode = "unrecorded"

// Armed reports whether the unmount veto is registered — the only value that
// means an eject will be held until the guest has shut down.
func (p Protection) Armed() bool { return p == ProtectionArmed }

// Degraded reports whether this state is a LOSS of protection on a host that
// could have had it, and therefore something to tell the user about.
//
// It is false for ProtectionArmed (nothing was lost), for ProtectionUnsupported
// (the host has no DiskArbitration; that is a platform fact, not a fault of
// this install or this cartridge) and for ProtectionUnrecorded (an older
// holder simply did not say, and inventing a fault from silence is the same
// class of lie this whole value exists to remove).
func (p Protection) Degraded() bool {
	switch p {
	case ProtectionNoCartridge, ProtectionNoDevNode, ProtectionUnreadableDevNode,
		ProtectionNoSession, ProtectionWatchFailed:
		return true
	default:
		return false
	}
}

// Code returns the stable machine-readable name of this state, for --json
// consumers that branch on it. It is the on-disk string, except that the zero
// value reports unrecordedCode instead of "".
func (p Protection) Code() string {
	if p == ProtectionUnrecorded {
		return unrecordedCode
	}
	return string(p)
}

// Reason returns the state in user language: a clause that completes a sentence
// about the cartridge, never a constant name. It is what the CLI prints and
// what the holder writes into its log, so the two cannot drift apart.
func (p Protection) Reason() string {
	switch p {
	case ProtectionArmed:
		return "an eject is held until the guest has shut down"
	case ProtectionUnrecorded:
		return "the process that started this instance did not record it"
	case ProtectionNoCartridge:
		return "no cartridge image is attached"
	case ProtectionNoDevNode:
		return "the attached cartridge reported no device node"
	case ProtectionUnreadableDevNode:
		return "the cartridge device node does not name a disk macOS can watch"
	case ProtectionNoSession:
		return "macOS DiskArbitration could not be reached"
	case ProtectionWatchFailed:
		return "macOS refused to register the eject watcher"
	case ProtectionUnsupported:
		return "eject protection needs macOS DiskArbitration, which this host does not have"
	default:
		return "the recorded reason " + string(p) + " is not one this build knows"
	}
}
