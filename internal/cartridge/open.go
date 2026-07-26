// Opening a cartridge: convert (when shipped as a read-only .dmg), attach,
// verify the layout, and load the packed manifest — as one VALUE that owns
// everything it created, so a process can hold several open cartridges at once
// and tear each one down independently.

package cartridge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/disk"
)

// ErrNoMountpoint is returned by Open when no mountpoint was supplied. A
// cartridge is always attached at a caller-chosen location (per-instance state
// lives inside it), so there is no sane default to fall back to.
var ErrNoMountpoint = errors.New("cartridge: no mountpoint given")

// Opened is a mounted, verified cartridge together with everything Open
// created on its behalf. Close releases it all, in the right order.
type Opened struct {
	// Name is the cartridge/slot name, derived from SourcePath unless the
	// caller overrode it.
	Name string
	// SourcePath is the image the caller named: the shipped .dmg or a runnable
	// .sparseimage. It is never mutated when it is a .dmg.
	SourcePath string
	// WorkingCopy is the writable .sparseimage materialized from a shipped
	// .dmg, removed by Close. Empty when SourcePath was already runnable.
	WorkingCopy string
	// Mount is the attached volume, including the BSD device node
	// DiskArbitration addresses it by.
	Mount Mount
	// Manifest is the packed disk manifest read from the cartridge.
	Manifest *disk.Manifest
	// Metadata is the cartridge self-description (format version, provenance).
	Metadata Metadata
	// Layout addresses the files inside the mounted volume.
	Layout Layout
}

// OpenOptions configures Open.
type OpenOptions struct {
	// Mountpoint is where the cartridge image is attached. Required: use
	// MountpointFor(stateDir, name) for the conventional private location.
	Mountpoint string
	// Name overrides the cartridge name derived from the image path.
	Name string
}

// Open makes a cartridge image bootable and returns it as an owned value.
//
// A shipped read-only .dmg is first converted to a writable working
// .sparseimage next to it (the AirDrop artifact stays pristine), then the image
// is attached at opts.Mountpoint, its layout verified, and its packed manifest
// loaded. Any failure after a step succeeded unwinds that step, so a failed
// Open leaves nothing attached and no stray working copy.
//
// The caller owns the result and MUST Close it once whatever is using the
// cartridge (the VMM holding root.img) has stopped.
func Open(path string, opts OpenOptions) (*Opened, error) {
	if !hostSupported() {
		return nil, ErrUnsupported
	}
	return open(context.Background(), defaultRunner, path, opts)
}

// open is the platform-neutral worker behind Open, taking a commandRunner so
// tests can drive the whole sequence without a real hdiutil. Each hdiutil step
// gets its own timeout derived from parent, matching the per-operation budgets
// the standalone wrappers use.
func open(parent context.Context, r commandRunner, path string, opts OpenOptions) (*Opened, error) {
	if opts.Mountpoint == "" {
		return nil, ErrNoMountpoint
	}
	name := opts.Name
	if name == "" {
		name = NameFromPath(path)
	}

	o := &Opened{Name: name, SourcePath: path}
	bootImg, err := o.materialize(parent, r, path)
	if err != nil {
		return nil, err
	}

	attachCtx, cancelAttach := context.WithTimeout(parent, attachTimeout)
	defer cancelAttach()
	mount, err := attach(attachCtx, r, bootImg, opts.Mountpoint)
	if err != nil {
		o.removeWorkingCopy()
		return nil, fmt.Errorf("attach cartridge: %w", err)
	}
	o.Mount = *mount
	o.Layout = NewLayout(mount.Mountpoint)

	if err := o.inspect(); err != nil {
		// Unwind the attach so a rejected cartridge never stays mounted.
		detachCtx, cancelDetach := context.WithTimeout(parent, detachTimeout)
		defer cancelDetach()
		_ = o.closeWith(detachCtx, r)
		return nil, err
	}
	return o, nil
}

// materialize resolves the image that will actually be attached. A shipped
// .dmg is converted to a writable working copy (recorded for Close); anything
// else is attached as-is.
func (o *Opened) materialize(parent context.Context, r commandRunner, path string) (string, error) {
	// Extension matching is case-sensitive on purpose: it mirrors HasImageExt,
	// which is what decided this path was a cartridge in the first place.
	if filepath.Ext(path) != DMGExt {
		return path, nil
	}
	// Clear any stale working copy first: a prior boot that crashed before
	// detach could have left one, and hdiutil convert refuses to overwrite, so
	// re-booting the same .dmg must not depend on a clean exit last time.
	work := TrimExt(path)
	_ = os.Remove(work + SparseExt)

	ctx, cancel := context.WithTimeout(parent, convertTimeout)
	defer cancel()
	converted, err := convertToSparse(ctx, r, path, work)
	if err != nil {
		return "", fmt.Errorf("convert cartridge dmg to working copy: %w", err)
	}
	o.WorkingCopy = converted
	return converted, nil
}

// inspect verifies the mounted volume really is a cartridge this build can read
// and loads its packed manifest.
func (o *Opened) inspect() error {
	meta, err := Verify(o.Mount.Mountpoint)
	if err != nil {
		return err
	}
	o.Metadata = meta
	m, err := o.Layout.LoadManifest()
	if err != nil {
		return err
	}
	o.Manifest = m
	return nil
}

// Close releases the cartridge: detach the volume, then remove the working copy
// materialized from a .dmg. Order matters — the working copy is the backing
// store of the mount, so it can only be removed once the volume is gone.
//
// Close is idempotent and safe on a partially-opened cartridge. It returns the
// detach error (removal of the working copy is best-effort: a leftover file is
// a wasted gigabyte, not a correctness problem, and the next Open clears it).
func (o *Opened) Close() error {
	if o == nil {
		return nil
	}
	if !hostSupported() {
		// Unreachable in practice: Open cannot produce an *Opened off darwin.
		return ErrUnsupported
	}
	ctx, cancel := context.WithTimeout(context.Background(), detachTimeout)
	defer cancel()
	return o.closeWith(ctx, defaultRunner)
}

// closeWith is the platform-neutral worker behind Close, taking the runner so
// the unwind path (and tests) can drive detach without a real hdiutil.
func (o *Opened) closeWith(ctx context.Context, r commandRunner) error {
	var err error
	if o.Mount.Mountpoint != "" {
		err = detach(ctx, r, o.Mount.Mountpoint)
		o.Mount = Mount{}
	}
	o.removeWorkingCopy()
	return err
}

// removeWorkingCopy deletes the .dmg-derived working image, if any.
func (o *Opened) removeWorkingCopy() {
	if o.WorkingCopy == "" {
		return
	}
	_ = os.Remove(o.WorkingCopy)
	o.WorkingCopy = ""
}

// Mountpoint returns where the cartridge is mounted, or "" if it is not.
func (o *Opened) Mountpoint() string {
	if o == nil {
		return ""
	}
	return o.Mount.Mountpoint
}

// StillAttached reports whether this exact cartridge is still mounted where
// Open put it — device-node precise, so an unrelated volume mounted over the
// same path cannot be mistaken for it.
func (o *Opened) StillAttached() bool {
	if o == nil {
		return false
	}
	return IsAttachedFrom(o.Mount.Mountpoint, o.Mount.DevNode)
}

// ApplyTo roots a VM config inside the mounted cartridge: the bootable
// root.img, EFI + cloud-init state under state/, and the read-write share under
// share/.
//
// It must be applied AFTER any manifest and flag overrides, so the cartridge's
// own image and state always win — a cartridge is by definition self-contained.
// Nothing here is host-specific, which is what makes a cartridge portable.
func (o *Opened) ApplyTo(cfg *config.Config) {
	if o == nil || cfg == nil || o.Mount.Mountpoint == "" {
		return
	}
	root := o.Layout.RootImagePath()

	// The cartridge carries its own image; every remote/base-image identity is
	// cleared so nothing can re-download or re-verify over it.
	cfg.BaseImagePath = root
	cfg.BaseImageURL = ""
	cfg.BaseImageSHA512 = ""
	cfg.BaseImageExpectedSHA256 = ""
	// root.img is already the resized disk, so DiskPath IS root.img: the VM
	// boots the cartridge's disk in place, with no copy or resize on boot.
	cfg.DiskPath = root

	cfg.EFIVarsPath = o.Layout.EFIVarsPath()
	cfg.CloudInitDir = o.Layout.CloudInitDir()

	// The read-write host<->guest share lives inside the cartridge too.
	cfg.ShareDir = o.Layout.ShareDir()
	cfg.ShareTag = ShareTag(o.Manifest)
	cfg.ShareGuestPath = ShareGuestPath(o.Manifest)
}

// GUI reports whether the cartridge's manifest asks for a GUI boot.
func (o *Opened) GUI() bool {
	return o != nil && o.Manifest != nil && o.Manifest.Boot.Mode == disk.BootModeGUI
}
