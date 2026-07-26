// Deciding whether an arbitrary mounted volume is a bootable bladerunner
// cartridge, and recovering the image file behind it.
//
// This is the entry point for goal 4 of the standalone-cartridge design: a
// volume appears (DiskArbitration hands us a mountpoint), and something has to
// answer "can I offer to boot this?" without a false positive on the user's
// Time Machine drive and without silently ignoring a cartridge that is merely
// damaged.
//
// The answer is deliberately THREE-VALUED, not boolean:
//
//   - StatusNotCartridge — none of our business. Say nothing.
//   - StatusUnbootable   — it IS a cartridge, but this build cannot boot it.
//     Reason says why (missing root.img, packed by a newer bladerunner, corrupt
//     manifest). The user mounted something that was meant to boot, so staying
//     silent would be the worst possible behavior.
//   - StatusBootable     — offer the boot.
//
// The layout half of the check is plain file I/O, so — like version.go and
// unlike the hdiutil-backed half — it is NOT gated on hostSupported() and stays
// unit-testable in Linux CI. Only the platform probes (statfs, `hdiutil info`)
// are skipped off darwin, and they are best-effort even there: a cartridge that
// verifies is bootable whether or not we could name its backing file.

package cartridge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stuffbucket/bladerunner/internal/disk"
)

// Status classifies what Detect found at a volume.
type Status string

const (
	// StatusNotCartridge means the volume is not a bladerunner cartridge and
	// never claimed to be. Callers should drop it without notifying anyone.
	StatusNotCartridge Status = "not-cartridge"
	// StatusUnbootable means the volume carries a cartridge manifest but is
	// not bootable by this build. Detected.Reason explains why, and it is
	// meant to be shown.
	StatusUnbootable Status = "unbootable"
	// StatusBootable means the volume holds a complete cartridge this build
	// can boot.
	StatusBootable Status = "bootable"
)

// Reasons reported for a volume that is not a cartridge at all. They exist so
// logs can explain a negative without the caller re-deriving it.
const (
	reasonUnreadable    = "cannot be inspected"
	reasonNotADirectory = "is not a directory"
	reasonNoManifest    = "has no " + ManifestFile + " at its root"
)

// Detected is the verdict on one mounted volume, rich enough for the caller to
// either boot it or tell the user precisely why it cannot.
//
// Every method is nil-receiver safe, so a caller can chain off a failed Detect
// without a nil check.
type Detected struct {
	// Status is the three-valued verdict. Branch on this, never on Err.
	Status Status
	// Name is the cartridge name: from cartridge.json, else the packed
	// manifest, else derived from the volume name. Empty when the volume is
	// not a cartridge.
	Name string
	// Mountpoint is the symlink-resolved directory the volume is mounted at.
	Mountpoint string
	// VolumeName is the last path element of Mountpoint, i.e. the name macOS
	// gave the volume ("bladerunner-demo").
	VolumeName string
	// Manifest is the packed disk manifest, non-nil only when Status is
	// StatusBootable (an unparseable manifest is itself a reason not to boot).
	Manifest *disk.Manifest
	// Metadata is the cartridge self-description, as far as it could be read.
	Metadata Metadata
	// FormatVersion is the on-image layout revision (see FormatVersion). A
	// cartridge with no cartridge.json reports the legacy version, not zero.
	FormatVersion int
	// ReadOnly reports whether the mounted volume is read-only — true for a
	// shipped UDZO .dmg, false for a runnable .sparseimage. It changes what
	// booting means: a read-only view has to be converted to a writable
	// working copy first, which is why BackingImage matters.
	ReadOnly bool
	// BackingImage is the .dmg/.sparseimage file behind the mount, recovered
	// from `hdiutil info`. Empty when the volume is not a mounted disk image
	// (or when hdiutil could not be consulted). This is the path to hand a
	// holder: it re-opens the source rather than booting a read-only view.
	BackingImage string
	// DevNode is the BSD device node backing the volume, e.g. /dev/disk9s1 —
	// the handle DiskArbitration and diskutil address it by. Empty off darwin.
	DevNode string
	// Reason is a human-readable explanation of a non-bootable verdict, safe
	// to show in a notification. Empty when Status is StatusBootable.
	Reason string
	// Err is the underlying error behind Reason, for programmatic matching
	// (errors.Is against ErrFormatTooNew or ErrNotCartridge). Nil when the
	// verdict needed no error.
	Err error
}

// Detect reports whether the volume mounted at volumePath is a bladerunner
// cartridge and, if so, whether this build can boot it.
//
// The cheap, authoritative filter comes first: a cartridge always carries its
// packed disk.json at the volume root, so one stat rejects everything else. A
// volume that passes it is then fully verified (layout, format version,
// manifest) and, on darwin, probed for its device node, read-only-ness, and
// backing image file.
//
// The error return is reserved for a caller mistake (an empty path); every
// outcome about the volume itself is reported in the returned Detected, which
// is never nil when err is nil. Use IsCandidate for the volume-name prefilter
// on a DiskArbitration callback before spending even this much work.
func Detect(volumePath string) (*Detected, error) {
	ctx, cancel := context.WithTimeout(context.Background(), infoTimeout)
	defer cancel()
	// Off darwin there is no hdiutil to ask; the layout verdict is still
	// meaningful, so the runner is simply withheld rather than the whole call
	// being refused.
	var r commandRunner
	if hostSupported() {
		r = defaultRunner
	}
	return detect(ctx, r, volumePath)
}

// detect is the platform-neutral worker behind Detect. A nil runner skips the
// hdiutil probe; tests pass a fake to drive it against captured output.
func detect(ctx context.Context, r commandRunner, volumePath string) (*Detected, error) {
	if volumePath == "" {
		return nil, ErrNoMountpoint
	}
	resolved := resolvePath(volumePath)
	d := &Detected{
		Status:     StatusNotCartridge,
		Mountpoint: resolved,
		VolumeName: filepath.Base(resolved),
	}

	if !d.isReadableDir() {
		return d, nil
	}
	if !d.hasManifest() {
		d.Reason = d.describe(reasonNoManifest)
		return d, nil
	}

	// The volume claims to be a cartridge. From here every failure is reported
	// as UNBOOTABLE with a reason rather than dropped, because the user
	// plugged in something that was meant to boot.
	d.classify()
	d.probeHost(ctx, r)
	return d, nil
}

// isReadableDir checks that the volume is a directory we can look inside,
// recording a not-a-cartridge reason when it is not. A stat failure here is
// never propagated: "that path is not a cartridge" is a complete answer, and
// the cause is kept in Err for the log.
func (d *Detected) isReadableDir() bool {
	info, err := os.Stat(d.Mountpoint)
	switch {
	case err != nil:
		d.Reason = d.describe(reasonUnreadable)
		d.Err = err
		return false
	case !info.IsDir():
		d.Reason = d.describe(reasonNotADirectory)
		return false
	}
	return true
}

// hasManifest reports whether the volume carries a packed disk.json at its
// root — the cheap, authoritative "is this ours" test.
func (d *Detected) hasManifest() bool {
	_, err := os.Stat(filepath.Join(d.Mountpoint, ManifestFile))
	return err == nil
}

// describe renders a not-a-cartridge reason against the volume it applies to.
func (d *Detected) describe(reason string) string {
	return fmt.Sprintf("%s %s", d.Mountpoint, reason)
}

// classify runs the full layout verification and records the verdict.
func (d *Detected) classify() {
	meta, err := Verify(d.Mountpoint)
	d.Metadata = meta
	d.FormatVersion = meta.FormatVersion
	if err != nil {
		d.unbootable(err)
		return
	}
	manifest, err := NewLayout(d.Mountpoint).LoadManifest()
	if err != nil {
		d.unbootable(err)
		return
	}
	d.Manifest = manifest
	d.Status = StatusBootable
	d.Name = d.resolveName()
}

// unbootable records a cartridge this build cannot boot, translating err into a
// reason the user can act on.
func (d *Detected) unbootable(err error) {
	d.Status = StatusUnbootable
	d.Err = err
	d.Name = d.resolveName()

	var layoutErr *LayoutError
	if errors.As(err, &layoutErr) {
		// LayoutError's own message says "is not a bladerunner cartridge",
		// which is wrong here: it IS one, it is incomplete.
		d.Reason = fmt.Sprintf("incomplete cartridge: missing %s",
			strings.Join(layoutErr.Missing, ", "))
		return
	}
	d.Reason = err.Error()
}

// resolveName picks the most authoritative cartridge name available: the
// metadata stamp, then the packed manifest, then the volume name.
func (d *Detected) resolveName() string {
	if d.Metadata.Name != "" {
		return d.Metadata.Name
	}
	if d.Manifest != nil && d.Manifest.Name != "" {
		return d.Manifest.Name
	}
	return NameFromVolume(d.VolumeName)
}

// probeHost fills in the platform-dependent fields. Every step is best-effort:
// a cartridge whose device node or backing file could not be determined is
// still a cartridge, and downgrading a good verdict over it would be wrong.
func (d *Detected) probeHost(ctx context.Context, r commandRunner) {
	// Only trust statfs when the path really is a mount root: for an ordinary
	// directory it answers for the CONTAINING filesystem, which would report
	// somebody else's device node and somebody else's read-only flag.
	if mi, err := lookupMount(d.Mountpoint); err == nil && mi.Mountpoint == d.Mountpoint {
		d.DevNode = mi.DevNode
		if readOnly, roErr := mountReadOnly(d.Mountpoint); roErr == nil {
			d.ReadOnly = readOnly
		}
	}
	if r == nil {
		return
	}
	// Prefer the device node: it is the kernel's own answer, whereas a
	// mountpoint match relies on hdiutil and statfs spelling the path alike.
	ref := d.DevNode
	if ref == "" {
		ref = d.Mountpoint
	}
	backing, err := backingImageFor(ctx, r, ref)
	if err != nil {
		return
	}
	d.BackingImage = backing.ImagePath
	if d.DevNode == "" {
		d.DevNode = backing.DevNode
	}
	if !backing.Writable {
		// hdiutil is authoritative about the IMAGE being read-only even where
		// statfs could not be consulted (or reported the containing volume).
		d.ReadOnly = true
	}
}

// Bootable reports whether the volume can be booted as it stands.
func (d *Detected) Bootable() bool {
	return d != nil && d.Status == StatusBootable
}

// Recognized reports whether the volume is a cartridge at all — bootable or
// merely damaged. It is the test for "should the user be told about this?".
func (d *Detected) Recognized() bool {
	return d != nil && d.Status != StatusNotCartridge
}

// BootSource is the path a holder should be started with: the backing image
// file when one was recovered (so the holder converts and attaches its own
// writable copy, leaving the shipped artifact pristine), else the mountpoint.
//
// It is empty when the volume is not a cartridge.
func (d *Detected) BootSource() string {
	if !d.Recognized() {
		return ""
	}
	if d.BackingImage != "" {
		return d.BackingImage
	}
	return d.Mountpoint
}

// String renders the verdict for a log line or a notification body.
func (d *Detected) String() string {
	if d == nil {
		return string(StatusNotCartridge)
	}
	switch d.Status {
	case StatusBootable:
		return fmt.Sprintf("cartridge %q at %s (format v%d, %s)",
			d.Name, d.Mountpoint, d.FormatVersion, d.access())
	case StatusUnbootable:
		return fmt.Sprintf("cartridge %q at %s cannot be booted: %s",
			d.Name, d.Mountpoint, d.Reason)
	case StatusNotCartridge:
		return fmt.Sprintf("%s is not a bladerunner cartridge", d.Mountpoint)
	default:
		return fmt.Sprintf("%s: %s", d.Mountpoint, d.Status)
	}
}

// access renders the mount's writability for String.
func (d *Detected) access() string {
	if d.ReadOnly {
		return "read-only"
	}
	return "writable"
}

// IsCandidate reports whether a volume name is worth looking at, using nothing
// but the name — no filesystem access at all.
//
// This is the hot-path filter for a DiskArbitration appeared-callback, which
// fires for every volume on the machine including ones we must not touch. It
// is intentionally permissive: a true answer only earns the volume a Detect,
// which is the authoritative check. Finder's collision suffix
// ("bladerunner-demo 1") passes, since it is still one of ours.
//
// A full path is accepted as well as a bare volume name, so a caller holding
// DiskInfo.VolumePath need not split it first.
func IsCandidate(volumeName string) bool {
	name := volumeBaseName(volumeName)
	return len(name) > len(VolumePrefix) && strings.HasPrefix(name, VolumePrefix)
}

// NameFromVolume derives a cartridge name from a mounted volume's name, undoing
// what VolumeName did at pack time: the bladerunner- prefix is stripped, as is
// the " 2" style suffix macOS appends when a volume of that name is already
// mounted. A name that carries no prefix is returned unchanged, so an
// unconventionally named cartridge still gets a usable label.
func NameFromVolume(volumeName string) string {
	name := volumeBaseName(volumeName)
	if name == "" {
		return ""
	}
	return trimMountCollisionSuffix(strings.TrimPrefix(name, VolumePrefix))
}

// volumeBaseName reduces a volume name or mountpoint to the volume name.
func volumeBaseName(volumeName string) string {
	if volumeName == "" {
		return ""
	}
	if strings.ContainsRune(volumeName, filepath.Separator) {
		return filepath.Base(volumeName)
	}
	return volumeName
}

// trimMountCollisionSuffix removes the " <n>" macOS appends to a volume name
// that is already mounted ("bladerunner-demo 1"). A name that is nothing but
// digits after the space, or has nothing before it, is left alone.
func trimMountCollisionSuffix(name string) string {
	idx := strings.LastIndexByte(name, ' ')
	if idx <= 0 || idx == len(name)-1 {
		return name
	}
	for i := idx + 1; i < len(name); i++ {
		if name[i] < '0' || name[i] > '9' {
			return name
		}
	}
	return name[:idx]
}
