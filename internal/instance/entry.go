// Package instance is the registry of running bladerunner VM instances.
//
// Historically a booted VM was discovered by scanning directories bladerunner
// itself had created (<state>/mnt/*, <state>/disks/*). That only works while
// the CLI owns the VM and every slot lives under the state dir. Once the VM is
// held by a standalone holder process that outlives the CLI — and once
// cartridges can be mounted anywhere — discovery needs a durable record.
//
// Each instance publishes one small JSON file under <stateDir>/instances/. The
// file is written atomically (temp + fsync + rename + dir fsync, the same
// pattern as internal/bootstage) so a concurrent reader never observes a
// half-written entry. Entries are advisory: they record where an instance
// lives and who holds it, and Alive gives a cheap liveness estimate. The
// authoritative answer is always "dial the control socket".
//
// This package is deliberately dependency-light (stdlib + internal/logging) so
// that both the CLI and the holder process can import it without cycles; in
// particular it must NOT import internal/control.
package instance

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// Kind describes how an instance was booted, which determines what its
// teardown has to clean up (a cartridge has a device node to detach, a disk
// slot has a working copy under the state dir, a flat instance has neither).
type Kind string

const (
	// KindFlat is the classic single-instance layout rooted directly at the
	// state dir (a plain `br start`).
	KindFlat Kind = "flat"
	// KindDisk is a slot materialized from a .disk manifest under
	// <state>/disks/<name>.
	KindDisk Kind = "disk"
	// KindCartridge is a slot backed by an attached cartridge DMG; its state
	// dir is the mountpoint and it owns a device node.
	KindCartridge Kind = "cartridge"
)

// Valid reports whether k is one of the known instance kinds.
func (k Kind) Valid() bool {
	switch k {
	case KindFlat, KindDisk, KindCartridge:
		return true
	default:
		return false
	}
}

// Ports records the host-side (localhost) ports an instance published, so a
// second instance can be given a non-conflicting set and so clients can find
// the right endpoint without re-deriving it from config.
type Ports struct {
	SSH  int `json:"ssh,omitempty"`
	API  int `json:"api,omitempty"`
	Web  int `json:"web,omitempty"`
	OIDC int `json:"oidc,omitempty"`
	NTP  int `json:"ntp,omitempty"`
}

// Entry is one instance's registry record: everything a process that did not
// start the VM needs in order to find it, talk to it, and tear it down.
type Entry struct {
	// Identity.
	//
	// Name is the slot name; it doubles as the registry file name and so must
	// satisfy ValidName. Kind selects the teardown path.
	Name string `json:"name"`
	Kind Kind   `json:"kind"`

	// Storage locations.
	//
	// StateDir is the instance's base directory — the control socket, disk and
	// logs live under it (for a cartridge this is the mountpoint). SourcePath
	// is the artifact the instance was booted from (a .disk manifest or a
	// cartridge DMG). WorkingCopy is the mutable disk image actually attached
	// to the VM, when it differs from SourcePath.
	StateDir    string `json:"stateDir"`
	SourcePath  string `json:"sourcePath,omitempty"`
	WorkingCopy string `json:"workingCopy,omitempty"`

	// Cartridge attachment.
	//
	// DevNode is the attached disk device (e.g. /dev/disk4) and Mountpoint is
	// where the DMG is mounted. Both are empty for non-cartridge kinds; both
	// are needed to detach cleanly on eject.
	DevNode    string `json:"devNode,omitempty"`
	Mountpoint string `json:"mountpoint,omitempty"`

	// Ownership.
	//
	// PID is the holder process that owns the VM. It is the primary liveness
	// signal; see Alive.
	PID int `json:"pid,omitempty"`

	// Endpoints.
	Ports Ports `json:"ports"`

	// Provenance.
	//
	// ProtocolVersion is the control-plane protocol the holder speaks, so a
	// newer CLI can refuse (or downgrade) rather than mis-parse. BinaryVersion
	// records the build that started the instance. StartedAt is when the
	// entry was published. GUI records whether the VM was booted with a
	// graphical window.
	ProtocolVersion int       `json:"protocolVersion,omitempty"`
	BinaryVersion   string    `json:"binaryVersion,omitempty"`
	StartedAt       time.Time `json:"startedAt"`
	GUI             bool      `json:"gui,omitempty"`
}

// ErrInvalidName is returned by ValidName (and wrapped by every function that
// takes a name) when a name cannot be used as a registry file name.
var ErrInvalidName = errors.New("invalid instance name")

// MaxNameLen bounds an instance name. Names become path elements on both the
// host state dir and a FAT/HFS-hosted cartridge, so keep them short.
const MaxNameLen = 64

// nameRe constrains an instance name to the same charset as a disk name
// (see internal/disk.ValidName): lowercase alphanumerics and dashes, starting
// with an alphanumeric. That rules out path separators, ".", "..", whitespace,
// leading dashes and NUL by construction.
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ValidName reports whether name is usable as an instance name — that is, as a
// single path element under the registry directory. It rejects the empty
// string, "." and "..", anything containing a path separator, and anything
// longer than MaxNameLen. Errors wrap ErrInvalidName.
func ValidName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%w: name is empty", ErrInvalidName)
	case name == "." || name == "..":
		return fmt.Errorf("%w: %q is not a usable path element", ErrInvalidName, name)
	case strings.ContainsRune(name, '/'), strings.ContainsRune(name, os.PathSeparator):
		return fmt.Errorf("%w: %q must not contain a path separator", ErrInvalidName, name)
	case len(name) > MaxNameLen:
		return fmt.Errorf("%w: %q is longer than %d characters", ErrInvalidName, name, MaxNameLen)
	case !nameRe.MatchString(name):
		return fmt.Errorf("%w: %q must match %s (lowercase letters, digits and dashes)", ErrInvalidName, name, nameRe.String())
	default:
		return nil
	}
}
