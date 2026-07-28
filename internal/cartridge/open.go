// Opening a cartridge: convert (when shipped as a read-only .dmg), attach,
// verify the layout, and load the packed manifest — as one VALUE that owns
// everything it created, so a process can hold several open cartridges at once
// and tear each one down independently.

package cartridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/disk"
)

// ErrNoMountpoint is returned when a mountpoint was required but not supplied.
// Under MountPrivate the cartridge is attached at a caller-chosen location
// (per-instance state lives inside it), so there is no sane default to fall
// back to. Under MountBrowsable no mountpoint is needed at all: macOS picks
// one and we read it back.
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

	// lock is the exclusive claim on the working copy, held for as long as this
	// cartridge is open. It is what stops a second boot from converting over —
	// or unlinking — an image this process is running from.
	lock *imageLock

	// persist records OpenOptions.Persist: whether Close COMMITS the working
	// copy back over SourcePath instead of discarding it. It is unexported
	// because the decision belongs to whoever opened the cartridge — flipping
	// it on a live *Opened would change what teardown does to a user's file.
	persist bool
}

// OpenOptions configures Open.
type OpenOptions struct {
	// Mountpoint is where the cartridge image is attached under MountPrivate;
	// use MountpointFor(stateDir, name) for the conventional private location.
	//
	// It is REQUIRED under MountPrivate and IGNORED under MountBrowsable, where
	// macOS chooses the location (and appends a collision suffix when a volume
	// of that name is already mounted). Read Opened.Mountpoint() for the
	// location that was actually used — never assume this field.
	Mountpoint string
	// Name overrides the cartridge name derived from the image path. It does
	// not rename the volume: the APFS volume name is baked in at pack time
	// (see VolumeName), which is what the mount-detection prefilter matches on.
	Name string
	// Policy selects the mount policy. The zero value is DefaultMountPolicy —
	// browsable, so the user can eject the cartridge in Finder and get an
	// orderly drain. Set MountPrivate for scripted or headless use that needs a
	// deterministic mountpoint.
	Policy MountPolicy
	// Persist asks Close to COMMIT the guest's changes back over the shipped
	// .dmg this cartridge was opened from, instead of discarding them with the
	// working copy.
	//
	// It is opt-in, and deliberately so: booting a .dmg has always been a
	// throwaway run, and a cartridge is a thing people hand to each other, so
	// overwriting one is a decision its owner has to make rather than a default
	// they discover after the fact. See Opened.writeBack for what it does and
	// what it refuses to do.
	//
	// It has no effect when SourcePath is already a runnable .sparseimage: the
	// guest writes into that file directly, so it is persistent either way.
	Persist bool
}

// Open makes a cartridge image bootable and returns it as an owned value.
//
// A shipped read-only .dmg is first converted to a writable working
// .sparseimage next to it (the AirDrop artifact stays pristine), then the image
// is attached under opts.Policy, its layout verified, and its packed manifest
// loaded. Any failure after a step succeeded unwinds that step, so a failed
// Open leaves nothing attached and no stray working copy.
//
// Under the default browsable policy the volume lands under /Volumes where the
// user can see and eject it; the mountpoint it actually got is
// Opened.Mountpoint(), which callers must use rather than predicting it.
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
	if !opts.Policy.Valid() {
		return nil, fmt.Errorf("cartridge: unknown mount policy %q", string(opts.Policy))
	}
	if opts.Policy.Private() && opts.Mountpoint == "" {
		return nil, ErrNoMountpoint
	}
	name := opts.Name
	if name == "" {
		name = NameFromPath(path)
	}

	o := &Opened{Name: name, SourcePath: path, persist: opts.Persist}

	// Claim the working copy BEFORE anything touches it. materialize deletes a
	// stale working copy and converts a fresh one over it; without this claim a
	// second boot of the same cartridge would unlink the image a live VMM is
	// running from, discarding every byte the first guest had written.
	if err := o.claim(); err != nil {
		return nil, err
	}

	bootImg, err := o.materialize(parent, r, path)
	if err != nil {
		o.releaseClaim()
		return nil, err
	}

	attachCtx, cancelAttach := context.WithTimeout(parent, attachTimeout)
	defer cancelAttach()
	mount, err := attach(attachCtx, r, attachRequest{
		path:       bootImg,
		mountpoint: opts.Mountpoint,
		policy:     opts.Policy,
	})
	if err != nil {
		o.removeWorkingCopy()
		o.releaseClaim()
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
//
// It is only ever reached with the working copy claimed (see Opened.claim), and
// it additionally refuses to clear a working copy that is still attached (see
// clearStaleWorkingCopy), so the removal below can never unlink an image
// anything is still running from.
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
	if err := clearStaleWorkingCopy(parent, r, work+SparseExt); err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(parent, convertTimeout)
	defer cancel()
	converted, err := convertToSparse(ctx, r, path, work)
	if err != nil {
		return "", fmt.Errorf("convert cartridge dmg to working copy: %w", err)
	}
	o.WorkingCopy = converted
	return converted, nil
}

// ErrWorkingCopyAttached reports that the working copy a boot would convert
// over is STILL ATTACHED: a volume the kernel serves from that file is mounted,
// even though no live process claims it. Booting anyway would unlink the
// backing store of a live mount, so it is refused.
var ErrWorkingCopyAttached = errors.New("cartridge working copy is still attached")

// clearStaleWorkingCopy removes the working copy left behind by an earlier
// boot, but only once nothing is reading from it any more.
//
// The flock claim is already held here, so no LIVE holder owns this image. That
// is not enough. flock is a KERNEL lock: when a holder is SIGKILLed (or
// force-terminated by `br stop --force`, which only cleans up the control
// socket) the kernel drops the lock, while nothing detaches the volume or
// removes the working copy. The image is then unclaimed AND attached, and
// unlinking it is the same data loss the claim exists to prevent, arriving
// through a different door.
//
// hdiutil is the only authority on the question, because the dead holder's
// mountpoint is not derivable — under the browsable policy macOS chose it. A
// lookup that cannot be completed is treated as "do not touch it": an unlink
// here is unrecoverable, and a boot that is refused with a reason is not.
func clearStaleWorkingCopy(parent context.Context, r commandRunner, work string) error {
	if _, err := os.Stat(work); errors.Is(err, os.ErrNotExist) {
		return nil // nothing left behind, so nothing can be attached from it
	}

	ctx, cancel := context.WithTimeout(parent, infoTimeout)
	defer cancel()
	attached, err := attachedImageAt(ctx, r, work)
	switch {
	case err == nil:
		return attachedWorkingCopyError(work, attached)
	case !errors.Is(err, ErrNoBackingImage):
		return fmt.Errorf("check whether %s is still attached: %w", work, err)
	}

	if err := os.Remove(work); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale working copy %s: %w", work, err)
	}
	return nil
}

// attachedWorkingCopyError refuses the boot in words the user can act on. A
// refusal that named neither the volume nor the way to release it would only
// trade one dead end for another: the whole point is that the user can detach
// the orphaned volume and boot again.
func attachedWorkingCopyError(work string, at *ImageBacking) error {
	where := "attached with no mounted volume"
	if at.Mountpoint != "" {
		where = "still mounted at " + at.Mountpoint
	}
	release := at.DevNode
	if release == "" {
		release = at.Mountpoint
	}
	return fmt.Errorf("%w: %s is %s, left behind by a holder that exited without detaching it; "+
		"eject that volume in Finder or run 'hdiutil detach %s', then boot again",
		ErrWorkingCopyAttached, work, where, release)
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

// Close releases the cartridge: detach the volume, then settle the working copy
// materialized from a .dmg — discarded by default, or committed back over the
// shipped file when the cartridge was opened with Persist. Order matters — the
// working copy is the backing store of the mount, so it can only be read or
// removed once the volume is gone.
//
// Close is idempotent and safe on a partially-opened cartridge. It returns the
// detach error joined with any write-back error; the working copy is settled
// only once the volume it backs is genuinely gone, so a leftover file after a
// failed detach is deliberate — the next Open clears it, or refuses if it is
// somehow still attached.
func (o *Opened) Close() error {
	if o == nil {
		return nil
	}
	if !hostSupported() {
		// Unreachable in practice: Open cannot produce an *Opened off darwin.
		return ErrUnsupported
	}
	ctx, cancel := context.WithTimeout(context.Background(), o.closeBudget())
	defer cancel()
	return o.closeWith(ctx, defaultRunner)
}

// closeWith is the platform-neutral worker behind Close, taking the runner so
// the unwind path (and tests) can drive detach without a real hdiutil.
//
// A FAILED detach keeps the working copy: it is the backing store of a volume
// the kernel is still serving, so removing it would be the same data loss
// clearStaleWorkingCopy refuses at the other end, and reading it to build a new
// cartridge would compress a disk mid-write. The Mount is kept too, so a later
// Close retries the detach rather than pretending the volume is gone.
//
// The claim on the working copy is released LAST, after the volume is gone and
// the working copy has been settled: until then another process must still be
// refused, or it would convert a fresh image over one this VMM is using — and
// with Persist it would do so while this process is still reading that image to
// build the cartridge it replaces.
func (o *Opened) closeWith(ctx context.Context, r commandRunner) error {
	var err error
	if o.Mount.Mountpoint != "" {
		if err = detach(ctx, r, o.Mount.Mountpoint); err == nil {
			o.Mount = Mount{}
		}
	}
	if o.Mount.Mountpoint == "" || !o.StillAttached() {
		err = errors.Join(err, o.settleWorkingCopy(ctx, r))
	}
	o.releaseClaim()
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

// Browsable reports whether the cartridge is mounted where the user can see and
// eject it in Finder. A holder uses this to decide whether a user-initiated
// unmount is even possible, and therefore whether registering an
// unmount-approval veto is worth doing.
func (o *Opened) Browsable() bool {
	return o != nil && o.Mount.Mountpoint != "" && o.Mount.Policy.Browsable()
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

// WritesBack reports whether Close will commit this cartridge's working copy
// back over the .dmg it was opened from.
//
// It is the conjunction a front end has to state to the user, not just the flag
// they passed: OpenOptions.Persist on a cartridge that was ALREADY runnable
// (a .sparseimage, which the guest writes into directly) leaves no working copy
// to commit, so nothing is written back and nothing needs to be.
func (o *Opened) WritesBack() bool {
	return o != nil && o.persist && o.WorkingCopy != ""
}

// WritesBack answers the same question for an image that has not been opened
// yet: will booting THIS path with persist set write anything back?
//
// A front end needs it because the boot it is announcing may be run by another
// process — a holder opens the cartridge itself — so there is no *Opened to
// ask. The two agree by construction: a working copy exists exactly when the
// source is a shipped .dmg, which is the test materialize makes.
func WritesBack(path string, persist bool) bool {
	return persist && filepath.Ext(path) == DMGExt
}

// --- the boot claim -------------------------------------------------------
//
// One cartridge image must be booted by at most one process at a time, and
// that has to hold no matter how the second boot was spelled: `br boot
// demo.dmg` and `br boot demo.sparseimage` name the SAME working copy, as does
// a second Finder mount of the same file under a different volume name. Nothing
// derived from a mountpoint can see that — the mountpoint is chosen by macOS —
// so the claim is keyed on the working-copy PATH and enforced by the kernel.
//
// flock(2) is used rather than an O_EXCL marker file because the kernel drops
// the lock when the holder dies, however it died. A crashed holder therefore
// leaves a stale lock FILE (harmless, and reused in place) but never a stale
// LOCK, which is the failure mode an exclusive-create scheme has to paper over
// with liveness probes it can only guess at.

// ErrCartridgeBusy reports that another live process already holds the working
// copy this cartridge would boot from. Booting anyway would convert a fresh
// image over the running VM's disk, so it is refused.
var ErrCartridgeBusy = errors.New("cartridge is already booted by another process")

const (
	// lockExt is appended to the (hidden) working-copy file name to form the
	// lock file, e.g. ".demo.sparseimage.lock" beside "demo.sparseimage".
	lockExt = ".lock"
	// lockFilePerm keeps the claim readable only by its owner; it records a PID
	// and an instance name.
	lockFilePerm = 0o600
)

// Holder identifies the process holding a cartridge's working copy. It is
// recorded inside the lock file so a refused boot can name the conflict
// instead of reporting an anonymous failure.
type Holder struct {
	// PID is the process that took the claim.
	PID int `json:"pid"`
	// Name is the instance name that process runs the cartridge under.
	Name string `json:"name,omitempty"`
	// Source is the image path it was booted from.
	Source string `json:"source,omitempty"`
}

// String renders a holder for a user-facing error.
func (h Holder) String() string {
	switch {
	case h.Name != "" && h.PID > 0:
		return fmt.Sprintf("instance %q (pid %d)", h.Name, h.PID)
	case h.Name != "":
		return fmt.Sprintf("instance %q", h.Name)
	case h.PID > 0:
		return "pid " + strconv.Itoa(h.PID)
	default:
		return "another process"
	}
}

// imageLock is a held claim on one working copy. The open file descriptor IS
// the lock, so it must stay open for as long as the cartridge is.
type imageLock struct {
	path string
	file *os.File
}

// lockPathFor returns the lock file guarding image: a hidden sibling of it, so
// the claim lives on the same filesystem as the thing it protects and needs no
// host state directory (a cartridge can be booted from anywhere, including a
// removable volume).
func lockPathFor(image string) string {
	canonical := CanonicalImagePath(image)
	return filepath.Join(filepath.Dir(canonical), "."+filepath.Base(canonical)+lockExt)
}

// claim takes the exclusive lock on this cartridge's working copy. It is a
// no-op when a claim is already held, so it is safe to call twice.
func (o *Opened) claim() error {
	if o == nil || o.lock != nil {
		return nil
	}
	lock, err := acquireImageLock(WorkingCopyPath(o.SourcePath), Holder{
		PID:    os.Getpid(),
		Name:   o.Name,
		Source: o.SourcePath,
	})
	if err != nil {
		return err
	}
	o.lock = lock
	return nil
}

// releaseClaim drops the claim, if one is held. It is idempotent.
func (o *Opened) releaseClaim() {
	if o == nil || o.lock == nil {
		return
	}
	o.lock.release()
	o.lock = nil
}

// acquireImageLock locks image for holder, or fails with ErrCartridgeBusy
// naming whoever holds it.
func acquireImageLock(image string, holder Holder) (*imageLock, error) {
	path := lockPathFor(image)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, lockFilePerm)
	if err != nil {
		return nil, fmt.Errorf("claim cartridge %s: %w", image, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		other, _ := readHolder(f)
		_ = f.Close()
		return nil, fmt.Errorf("%w: %s is held by %s", ErrCartridgeBusy, image, other)
	}
	writeHolder(f, holder)
	return &imageLock{path: path, file: f}, nil
}

// release closes the descriptor, which is what drops the kernel lock. The
// record is blanked first so a later reader never attributes the image to a
// process that has let go of it. The lock FILE is deliberately left behind:
// unlinking it would let a second process create and lock a different inode
// for the same path while this one still believes it holds the claim.
func (l *imageLock) release() {
	if l == nil || l.file == nil {
		return
	}
	_ = l.file.Truncate(0)
	_ = l.file.Close()
	l.file = nil
}

// writeHolder records who holds the claim. Best effort: the lock is already
// held at this point, and an unwritable record costs a good error message
// somewhere else, never correctness.
func writeHolder(f *os.File, holder Holder) {
	data, err := json.Marshal(holder)
	if err != nil {
		return
	}
	if err := f.Truncate(0); err != nil {
		return
	}
	_, _ = f.WriteAt(data, 0)
	_ = f.Sync()
}

// readHolder decodes the record from a lock file. A missing, empty or
// unparseable record yields the zero Holder, which still renders as "another
// process".
func readHolder(f *os.File) (Holder, error) {
	data := make([]byte, maxHolderRecord)
	n, err := f.ReadAt(data, 0)
	if n == 0 {
		return Holder{}, err
	}
	var h Holder
	if err := json.Unmarshal(data[:n], &h); err != nil {
		return Holder{}, err
	}
	return h, nil
}

// maxHolderRecord bounds the lock-file read. The record is a three-field JSON
// object; anything larger is not one of ours.
const maxHolderRecord = 4096

// Busy reports whether a live process is currently booted from the working copy
// that booting sourcePath would use, and who that is.
//
// It is a PROBE, not a reservation: the answer can go stale the instant it is
// returned, so it exists to give a friendly refusal before a destructive step
// (unmounting a volume, converting an image), never as the protection itself.
// The protection is the claim Open takes, which is atomic.
func Busy(sourcePath string) (Holder, bool) {
	if sourcePath == "" {
		return Holder{}, false
	}
	// Read-only, no O_CREATE: a probe must not litter a lock file beside an
	// image nobody ever booted. No lock file means no holder, ever.
	f, err := os.Open(lockPathFor(WorkingCopyPath(sourcePath)))
	if err != nil {
		return Holder{}, false
	}
	defer func() { _ = f.Close() }()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder, _ := readHolder(f)
		return holder, true
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return Holder{}, false
}
