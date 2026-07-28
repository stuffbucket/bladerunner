package main

import (
	"fmt"
	"io"

	"github.com/stuffbucket/bladerunner/internal/instance"
)

// Reporting of the cartridge unmount veto — goal 5 of the cartridge design,
// "orderly spin-down when the cartridge is ejected".
//
// The veto fails OPEN: a holder that cannot register the DiskArbitration
// unmount-approval callback logs a warning and runs the VM anyway. That is the
// right call (a missing safety net must not cost the user their VM) but it
// leaves a cartridge whose eject in Finder yanks the disk out from under a
// live guest looking exactly like one whose eject drains it first. The holder
// is a different process, so its log is not something the user sees. These
// helpers are what put the difference on screen.

// unmountProtectionReport is the --json view of one instance's eject
// protection. Code is the stable name to branch on; Reason is the sentence to
// show a person. Both come from internal/instance, which owns the vocabulary
// because it is what the registry record on disk stores.
type unmountProtectionReport struct {
	Protected bool   `json:"protected"`
	Code      string `json:"code"`
	Reason    string `json:"reason"`

	// state is the registry value the three exported fields are derived from.
	// It is kept so the human renderers do not have to parse Code back into a
	// Protection — a round trip that is lossy, because Code names the zero
	// value ("unrecorded") that the on-disk spelling leaves empty.
	state instance.Protection
}

// protectionReportFor builds the JSON view, or nil when there is nothing to
// report. Only a cartridge has a mount to protect, so a flat or disk-slot
// instance omits the object entirely rather than carrying a field that would
// read as a defect it cannot have.
func protectionReportFor(kind instance.Kind, p instance.Protection) *unmountProtectionReport {
	if kind != instance.KindCartridge {
		return nil
	}
	return &unmountProtectionReport{Protected: p.Armed(), Code: p.Code(), Reason: p.Reason(), state: p}
}

// protectionLabel is the one-word state, used both as a table cell and as a
// status row. It is deliberately short: the reason is a separate line.
func protectionLabel(p instance.Protection) string {
	switch {
	case p.Armed():
		return "protected"
	case p.Degraded():
		// Shouted, because this is the case that costs the user data and the
		// one the whole mechanism exists to prevent going unnoticed.
		return "UNPROTECTED"
	case p == instance.ProtectionUnsupported:
		return "unavailable"
	default:
		return "unknown"
	}
}

// protectionCell renders the EJECT column of `br instances`. A non-cartridge
// instance has no mount to protect, so it shows the same missingCell every
// other inapplicable column uses instead of inventing a state for it.
func protectionCell(p *unmountProtectionReport) string {
	if p == nil {
		return missingCell
	}
	return protectionLabel(p.state)
}

// protectionStatusValue is the styled `br status` value for the state. Only a
// degradation of a host that could have been protected is colored as a
// warning: a host with no DiskArbitration at all, and a record that predates
// the field, are stated in the subtle style so neither reads as a fault.
func protectionStatusValue(p instance.Protection) string {
	switch {
	case p.Armed():
		return success(protectionLabel(p))
	case p.Degraded():
		return warning(protectionLabel(p))
	default:
		return subtle(protectionLabel(p))
	}
}

// protectionNote is the sentence printed under the table for a cartridge that
// is not protected, or "" when there is nothing to say.
//
// "is off" and "is unknown" are kept apart on purpose: an entry written before
// the holder recorded this field proves nothing either way, and reporting
// silence as a fault is the same class of lie as reporting it as protection.
func protectionNote(name string, p instance.Protection) string {
	switch {
	case p.Armed():
		return ""
	case p == instance.ProtectionUnrecorded:
		return fmt.Sprintf("%s: eject protection is unknown - %s.", name, p.Reason())
	default:
		return fmt.Sprintf("%s: eject protection is off - %s.", name, p.Reason())
	}
}

// unprotectedConsequence says what the missing veto actually costs, once, under
// whatever notes were printed. Without it the notes name a mechanism the reader
// has no reason to care about.
const unprotectedConsequence = "Ejecting the cartridge in Finder will not shut its guest down first; use 'br eject' instead."

// writeProtectionNotes prints the reason line for every listed cartridge that
// is not protected, followed by one shared consequence line. It writes nothing
// when every cartridge is protected (and when there are no cartridges at all),
// so the common case stays quiet.
//
// The marker is chosen per note, not per block: a degradation of a host that
// could have been protected is a warning, while a platform that never had
// DiskArbitration and a record that predates the field are remarks. Marking
// all of them alike would make the one line worth acting on invisible among
// the ones that are not.
func writeProtectionNotes(out io.Writer, listings []instanceListing) error {
	lines := make([]string, 0, len(listings))
	for i := range listings {
		l := &listings[i]
		if l.UnmountProtection == nil {
			continue
		}
		p := l.UnmountProtection.state
		note := protectionNote(l.Name, p)
		if note == "" {
			continue
		}
		marker := subtle("·")
		if p.Degraded() {
			marker = warning("!")
		}
		lines = append(lines, fmt.Sprintf("  %s %s", marker, note))
	}
	if len(lines) == 0 {
		return nil
	}

	if _, err := fmt.Fprintln(out); err != nil {
		return err
	}
	for _, line := range lines {
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(out, "    %s\n", subtle(unprotectedConsequence))
	return err
}

// writeUnprotectedCartridge tells the user, at the one moment they are looking
// at the instance they just started, that its cartridge lost the eject veto.
//
// Only a DEGRADATION is printed. A host with no DiskArbitration at all has
// nothing to lose and nothing to act on, and a boot always records a state, so
// there is no "unknown" case here either; announcing those on every boot would
// train the reader to skip the line that matters. Until now the loss appeared
// nowhere but the holder's log, which for `br boot` is a file the user has no
// reason to open.
func writeUnprotectedCartridge(out io.Writer, p *unmountProtectionReport) error {
	if p == nil || !p.state.Degraded() {
		return nil
	}
	if _, err := fmt.Fprintf(out, "%s %s %s\n",
		warning("⚠"), key("Eject:"), warning("cartridge eject protection is off")); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "  %s %s\n", key("Reason:"), p.Reason); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "  %s %s\n\n", key("Effect:"), subtle(unprotectedConsequence)); err != nil {
		return err
	}
	return nil
}
