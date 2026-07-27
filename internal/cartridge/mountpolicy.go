// Mount policy: where a cartridge volume lands, and whether a human can see it.
//
// Historically a cartridge was always attached `-nobrowse` at
// <state>/mnt/<name>, deliberately invisible in Finder. Goals 4 and 5 of the
// standalone-cartridge design invert that: "eject the cartridge in Finder" IS
// the gesture that triggers an orderly VM spin-down, and a volume nobody can
// see cannot be ejected at all. So the default is now MountBrowsable — hdiutil
// picks the location under /Volumes and we read the REAL mountpoint back out of
// its plist, which is also what makes macOS's collision suffixing
// ("bladerunner-demo 1") work for free instead of failing an attach.
//
// MountPrivate keeps the old behavior verbatim, because a deterministic,
// caller-dictated mountpoint is exactly what CI, scripts/smoke-cartridge.sh,
// headless use and `br disk pack` need.
//
// The split also keeps pack and boot off each other's mountpoint: packing
// attaches privately under the state dir, while a booted cartridge lands under
// /Volumes, so the pack-vs-boot collision listed as a risk in the design cannot
// arise through this package.
//
// Everything here is plain argument arithmetic and plist selection, so — like
// layout.go and version.go — it is NOT gated on hostSupported() and stays
// unit-testable in Linux CI.

package cartridge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// MountPolicy decides where a cartridge volume is attached and whether it is
// visible in Finder. The zero value means DefaultMountPolicy, so a caller that
// does not care gets the behavior the design calls for.
type MountPolicy string

const (
	// MountBrowsable lets macOS place the volume under /Volumes and leaves it
	// visible in Finder, so the user can eject it — the gesture that drives an
	// orderly VM drain. The mountpoint is NOT dictated; it is read back from
	// `hdiutil attach -plist`.
	MountBrowsable MountPolicy = "browsable"
	// MountPrivate attaches `-nobrowse` at a caller-dictated mountpoint (see
	// MountpointFor). Invisible in Finder and therefore not ejectable by hand,
	// but deterministic — which is what scripted and headless use needs.
	MountPrivate MountPolicy = "private"
)

// DefaultMountPolicy is the policy applied to the zero value of MountPolicy.
const DefaultMountPolicy = MountBrowsable

// VolumesRoot is where macOS mounts browsable volumes.
const VolumesRoot = "/Volumes"

// hdiutil attach flags. They are hoisted because both policies share most of
// them and the tests assert on the exact argument vector.
const (
	flagMountpoint = "-mountpoint"
	flagNoBrowse   = "-nobrowse"
	flagOwners     = "-owners"
	flagNoVerify   = "-noverify"
	ownersOn       = "on"
)

// ErrMountpointUnknown is returned when a browsable attach reported success but
// hdiutil's plist did not name a mounted volume, so there is no way to know
// where the cartridge landed. Unlike the private policy — where the mountpoint
// was dictated and can simply be assumed — a browsable attach has nothing to
// fall back on, so this is fatal rather than best-effort.
var ErrMountpointUnknown = errors.New("cartridge: hdiutil reported no mounted volume")

// Resolve maps the zero value onto DefaultMountPolicy and leaves every named
// policy alone. Every branch on a policy goes through it.
func (p MountPolicy) Resolve() MountPolicy {
	if p == "" {
		return DefaultMountPolicy
	}
	return p
}

// Browsable reports whether the policy leaves the volume visible in Finder (and
// therefore ejectable by the user).
func (p MountPolicy) Browsable() bool { return p.Resolve() == MountBrowsable }

// Private reports whether the policy attaches at a caller-dictated mountpoint,
// hidden from Finder.
func (p MountPolicy) Private() bool { return p.Resolve() == MountPrivate }

// Valid reports whether p names a policy this build understands. The zero value
// is valid and means DefaultMountPolicy.
func (p MountPolicy) Valid() bool {
	switch p.Resolve() {
	case MountBrowsable, MountPrivate:
		return true
	default:
		return false
	}
}

// String renders the effective policy, so the zero value prints as the default
// rather than as an empty string.
func (p MountPolicy) String() string { return string(p.Resolve()) }

// MountPolicyFor maps a `--private-mount` style boolean onto a policy, so a
// flag owner never has to name the constants.
func MountPolicyFor(private bool) MountPolicy {
	if private {
		return MountPrivate
	}
	return MountBrowsable
}

// BrowsableMountpointFor returns where macOS is EXPECTED to mount a cartridge
// of the given name under the browsable policy: /Volumes/bladerunner-<name>.
//
// It is a prediction, not an authority. When a volume of that name is already
// mounted macOS appends a collision suffix ("bladerunner-demo 1"), which is
// precisely why the real mountpoint is always read back from the attach plist.
// Use this for a pre-attach liveness probe or a human-facing message, never as
// a mount target.
func BrowsableMountpointFor(name string) string {
	return filepath.Join(VolumesRoot, VolumeName(name))
}

// attachRequest describes one `hdiutil attach`. It exists so the argument
// vector, the pre-attach directory work, and the post-attach mountpoint
// discovery all branch on a single value rather than on scattered booleans.
type attachRequest struct {
	// path is the image file to attach.
	path string
	// mountpoint is the dictated mount location. Required under MountPrivate;
	// IGNORED under MountBrowsable, where macOS chooses.
	mountpoint string
	// policy selects the mount behavior; the zero value is the default.
	policy MountPolicy
}

// attachArgs builds the `hdiutil attach` argument vector for a request.
//
// Under MountPrivate it dictates the location and marks the volume
// non-browsable — byte-for-byte what this package has always emitted. Under
// MountBrowsable neither flag is passed: hdiutil's own defaults (mount under
// /Volumes, browsable) are exactly what we want, and there is no `-browse` to
// state it positively.
//
// `-plist` is what makes the browsable policy possible at all: it is how
// hdiutil describes every device node it created, and therefore how we learn
// both the volume's BSD dev-entry and — when we did not dictate it — its
// mountpoint.
func attachArgs(req attachRequest) []string {
	args := []string{cmdAttach, req.path}
	if req.policy.Private() {
		args = append(args, flagMountpoint, req.mountpoint, flagNoBrowse)
	}
	return append(args, flagOwners, ownersOn, flagNoVerify, flagPlist)
}

// attachPrivate mounts the image at the dictated mountpoint (creating the
// directory first) and returns a Mount whose Mountpoint is symlink-resolved for
// reliable later comparison and whose DevNode names the BSD device backing the
// volume. DevNode is best-effort: a cartridge that mounted successfully is
// still usable if hdiutil's plist was unparseable, so a lookup failure never
// fails the attach.
func attachPrivate(ctx context.Context, r commandRunner, req attachRequest) (*Mount, error) {
	if req.mountpoint == "" {
		return nil, ErrNoMountpoint
	}
	if err := os.MkdirAll(req.mountpoint, mountpointDirPerm); err != nil {
		return nil, fmt.Errorf("create mountpoint %q: %w", req.mountpoint, err)
	}
	out, errOut, err := r.run(ctx, hdiutil, attachArgs(req)...)
	if err != nil {
		return nil, wrapHdiutil(cmdAttach, err, errOut)
	}
	resolved := resolvePath(req.mountpoint)
	m := &Mount{
		Path:       req.path,
		Mountpoint: resolved,
		DevNode:    attachedDevNode(out, req.mountpoint),
		Policy:     MountPrivate,
	}
	if m.DevNode == "" && isMountpoint(resolved) {
		// hdiutil's plist is advisory here; ask the kernel which device backs
		// the volume we just mounted. Guarded by isMountpoint so we never
		// report the *parent* filesystem's device for a directory with nothing
		// on it.
		if info, lookupErr := lookupMount(resolved); lookupErr == nil {
			m.DevNode = info.DevNode
		}
	}
	return m, nil
}

// attachBrowsable lets macOS place the volume and learns where it went from
// hdiutil's plist.
//
// The plist is load-bearing rather than additive on this path, so an attach we
// cannot locate is unwound (any device node the image did produce is detached)
// and reported, instead of leaving a stranded volume nobody knows the path of.
func attachBrowsable(ctx context.Context, r commandRunner, req attachRequest) (*Mount, error) {
	out, errOut, err := r.run(ctx, hdiutil, attachArgs(req)...)
	if err != nil {
		return nil, wrapHdiutil(cmdAttach, err, errOut)
	}
	entities, parseErr := parseAttachEntities(out)
	if parseErr != nil {
		return nil, fmt.Errorf("%w: %w", ErrMountpointUnknown, parseErr)
	}
	entity, ok := selectMountedEntity(entities)
	if !ok {
		unwindAttach(ctx, r, entities)
		return nil, ErrMountpointUnknown
	}

	resolved := resolvePath(entity.MountPoint)
	m := &Mount{
		Path:       req.path,
		Mountpoint: resolved,
		DevNode:    entity.DevEntry,
		Policy:     MountBrowsable,
	}
	if m.DevNode == "" {
		// A plist that named a mountpoint but no dev-entry is unexpected but
		// harmless: the kernel can name the device for a path that IS mounted.
		if info, lookupErr := lookupMount(resolved); lookupErr == nil {
			m.DevNode = info.DevNode
		}
	}
	return m, nil
}

// selectMountedEntity picks the volume hdiutil mounted. A cartridge image holds
// exactly one mountable volume — the other entities are the partition scheme
// and the APFS container — so the first entity carrying a mount-point is it.
func selectMountedEntity(entities []systemEntity) (systemEntity, bool) {
	for _, e := range entities {
		if e.MountPoint != "" {
			return e, true
		}
	}
	return systemEntity{}, false
}

// unwindAttach detaches whatever an unlocatable attach did produce, so a
// browsable attach we could not make sense of never strands a device. Every
// step is best-effort: we are already returning an error, and a detach that
// also fails must not mask it.
func unwindAttach(ctx context.Context, r commandRunner, entities []systemEntity) {
	for _, e := range entities {
		if e.DevEntry == "" {
			continue
		}
		// The first dev-entry is the whole disk; detaching it releases every
		// entity the image produced.
		_ = detachWithBackoff(ctx, r, e.DevEntry, 0)
		return
	}
}
