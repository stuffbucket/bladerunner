// Writing a booted cartridge back over the .dmg it shipped as.
//
// Booting a .dmg has always been a throwaway run: the shipped artifact is
// read-only, so Open converts a writable working copy beside it and Close
// unlinks that copy, taking every byte the guest wrote with it. That is still
// the default. OpenOptions.Persist opts INTO keeping the changes, and this file
// is what that costs.
//
// The whole design is one rule: the user's cartridge is either the file they
// had or the complete new one, and never anything in between. So nothing here
// writes into SourcePath. A fresh artifact is built at a hidden staging path in
// the same directory, verified, and published with a single rename — the only
// step that touches the original at all, and the last one. Every failure before
// it leaves the original bit-identical, which is what the tests assert with a
// sha256 taken on both sides.
//
// The second rule is that a failed write-back must not also destroy the work it
// failed to save. The working copy IS the guest's disk, so a write-back that
// cannot finish moves it aside to a rescue name the next boot will not clear,
// and says where it went.
//
// Like the rest of the package the workers take a commandRunner, so the whole
// sequence — including every refusal — is unit-testable without hdiutil.

package cartridge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/stuffbucket/bladerunner/internal/util"
)

// cmdVerify is the hdiutil subcommand that checksums an image. It is what makes
// "verified" in the write-back sequence mean something: a convert that exited 0
// having produced a truncated file is exactly the case a stat cannot catch.
const cmdVerify = "verify"

// verifyTimeout bounds `hdiutil verify`. It reads the whole image, so it is
// budgeted like the convert that produced it rather than like a metadata probe.
const verifyTimeout = 30 * time.Minute

// commitPrefix and commitSuffix bracket the staging file name. The leading dot
// hides a half-built artifact from Finder and from `br boot *.dmg` globbing;
// the suffix keeps it from colliding with the cartridge it will replace.
const (
	commitPrefix = "."
	commitSuffix = ".commit"
)

// rescueInfix and rescueTimeLayout name the image a failed write-back leaves
// the guest's changes in. The name is deliberately NOT the working-copy path:
// clearStaleWorkingCopy removes that one on the next boot, which is correct for
// a stale copy and would be data loss for a rescued one.
const (
	rescueInfix      = "-rescue-"
	rescueTimeLayout = "20060102-150405"
	// maxRescueAttempts bounds the search for an unused rescue name. Two
	// write-backs failing inside one second is not a case worth a loop without
	// an end.
	maxRescueAttempts = 100
)

// writeProbePrefix names the throwaway file that answers "can we publish here
// at all?" before the expensive work starts.
const writeProbePrefix = ".bladerunner-writable-"

// ErrWriteBackAttached reports that a write-back was refused because the
// cartridge image is — or cannot be confirmed not to be — still attached.
//
// It is the "the VM is still running" refusal expressed in the only terms this
// package can verify: a running guest holds an attached volume, so an image
// nothing is attached to is an image nothing is running from. Reading a live
// backing store would compress a disk mid-write and ship the result as a
// cartridge.
var ErrWriteBackAttached = errors.New("cartridge: refusing to write back while the image is attached")

// ErrWriteBackReadOnly reports that the cartridge cannot be replaced where it
// sits — it is on a read-only volume, or in a directory this user cannot write.
// It is raised BEFORE the compress, so a doomed write-back costs seconds rather
// than half an hour.
var ErrWriteBackReadOnly = errors.New("cartridge: cannot replace the cartridge in place")

// ErrWriteBackFailed reports that building or publishing the new cartridge
// failed. It exists so callers — and users reading the message — can rely on
// the one fact that matters: the original cartridge is unchanged.
var ErrWriteBackFailed = errors.New("cartridge write-back failed; the original cartridge is unchanged")

// closeBudget is how long Close may take. A discarding close is a detach; a
// committing one additionally probes, compacts, compresses and verifies a whole
// disk image, and a budget that did not cover the convert would kill the
// write-back on exactly the cartridges big enough for anyone to care about.
func (o *Opened) closeBudget() time.Duration {
	if o == nil || !o.persist || o.WorkingCopy == "" {
		return detachTimeout
	}
	return detachTimeout + infoTimeout + compactTimeout + convertTimeout + verifyTimeout
}

// settleWorkingCopy disposes of the .dmg-derived working copy now that the
// volume it backed is gone: COMMITTED back over the source cartridge when this
// cartridge was opened with Persist, and discarded otherwise.
//
// It is the one place the two outcomes are chosen between, so "the default is
// discard" is a property of a single branch rather than of every caller.
func (o *Opened) settleWorkingCopy(ctx context.Context, r commandRunner) error {
	if err := o.writeBack(ctx, r); err != nil {
		return o.rescueWorkingCopy(err)
	}
	o.removeWorkingCopy()
	return nil
}

// writeBack commits the guest's changes back over the shipped .dmg.
//
// The sequence is refuse-first: everything that can say no does so before any
// bytes are produced, because the expensive step is the one that cannot be
// undone cheaply. Then compact (best effort), convert to a staging artifact,
// verify it, and publish it with one rename.
//
// It returns nil — doing nothing — when the cartridge was not opened with
// Persist, or when it has no working copy because the source was already a
// runnable .sparseimage the guest wrote into directly.
func (o *Opened) writeBack(parent context.Context, r commandRunner) error {
	if o == nil || !o.persist || o.WorkingCopy == "" {
		return nil
	}
	if err := o.confirmDetached(parent, r); err != nil {
		return err
	}
	dst := o.SourcePath
	if err := confirmReplaceable(dst); err != nil {
		return err
	}

	// Compaction only reclaims the blocks the guest freed. A cartridge that
	// cannot be compacted is still a cartridge, so a failure here costs the
	// user megabytes in the shipped file and is not allowed to cost them the
	// write-back itself.
	compactWorkingCopy(parent, r, o.WorkingCopy)

	staged, err := buildCommitArtifact(parent, r, o.WorkingCopy, commitStem(dst))
	if err != nil {
		return fmt.Errorf("%w: %w", ErrWriteBackFailed, err)
	}
	if err := util.PublishFileAtomic(staged, dst); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("%w: %w", ErrWriteBackFailed, err)
	}
	return nil
}

// confirmDetached refuses the write-back unless nothing is attached from the
// working copy.
//
// Two questions, and BOTH have to answer no. The recorded Mount says whether
// this process's own detach succeeded; hdiutil says whether anyone else's
// volume is still being served from that file — a second holder, a Finder mount
// the user made by hand, a device a crashed process never released. A probe
// that cannot be completed counts as "attached": overwriting a user's cartridge
// from an image we could not confirm is quiescent is not a risk worth taking to
// save them one retry.
func (o *Opened) confirmDetached(parent context.Context, r commandRunner) error {
	if o.Mount.Mountpoint != "" {
		return fmt.Errorf("%w: %s is still mounted at %s, so the detach has not finished",
			ErrWriteBackAttached, o.WorkingCopy, o.Mount.Mountpoint)
	}
	ctx, cancel := context.WithTimeout(parent, infoTimeout)
	defer cancel()

	at, err := attachedImageAt(ctx, r, o.WorkingCopy)
	switch {
	case err == nil:
		return fmt.Errorf("%w: %s is %s; eject that volume and boot again to write the changes back",
			ErrWriteBackAttached, o.WorkingCopy, attachmentLocation(at))
	case errors.Is(err, ErrNoBackingImage):
		return nil
	default:
		return fmt.Errorf("%w: could not confirm %s is detached: %w", ErrWriteBackAttached, o.WorkingCopy, err)
	}
}

// attachmentLocation renders where an image is still attached, for a refusal
// the user can act on.
func attachmentLocation(at *ImageBacking) string {
	if at.Mountpoint != "" {
		return "still mounted at " + at.Mountpoint
	}
	if at.DevNode != "" {
		return "still attached as " + at.DevNode
	}
	return "still attached"
}

// confirmReplaceable checks that the cartridge's directory accepts new files,
// which is what the staging write and the rename both need. The FILE's own mode
// is not the question: a rename needs write permission on the directory.
//
// It probes by creating and removing a file rather than by reading permission
// bits, because those bits do not answer it — a read-only mount, an ACL, or a
// full filesystem all say "no" only when something is actually written.
func confirmReplaceable(dst string) error {
	dir := filepath.Dir(dst)
	f, err := os.CreateTemp(dir, writeProbePrefix)
	if err != nil {
		return fmt.Errorf("%w: %s is not writable: %w", ErrWriteBackReadOnly, dir, err)
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return nil
}

// compactWorkingCopy reclaims the blocks the guest freed, best effort. See
// writeBack for why its error is not fatal.
func compactWorkingCopy(parent context.Context, r commandRunner, work string) {
	ctx, cancel := context.WithTimeout(parent, compactTimeout)
	defer cancel()
	_ = compact(ctx, r, work)
}

// commitStem returns the hidden staging path the new cartridge is built at: a
// sibling of the cartridge it will replace, so publishing it is a rename within
// one filesystem rather than a copy across two, and hidden so a half-built
// artifact is never mistaken for a cartridge. hdiutil appends the extension.
func commitStem(dst string) string {
	dir, base := filepath.Split(TrimExt(dst))
	return filepath.Join(dir, commitPrefix+base+commitSuffix)
}

// buildCommitArtifact compresses the working copy into a verified staging
// artifact and returns its path. Anything it produced but could not stand
// behind is removed before it returns an error, so a failure leaves the
// cartridge's directory as it found it.
func buildCommitArtifact(parent context.Context, r commandRunner, work, stem string) (string, error) {
	// hdiutil convert refuses to overwrite, so a staging file left by a
	// write-back that was interrupted (Ctrl+C, a crash, a lost power) would
	// otherwise break persistence permanently rather than for one run.
	if err := os.Remove(stem + DMGExt); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("clear the staging artifact %s left by an interrupted write-back: %w", stem+DMGExt, err)
	}

	out, err := convertWorkingCopy(parent, r, work, stem)
	if err != nil {
		return "", fmt.Errorf("compress %s into a cartridge: %w", work, err)
	}
	if err := verifyArtifact(parent, r, out); err != nil {
		_ = os.Remove(out)
		return "", err
	}
	return out, nil
}

// convertWorkingCopy compresses work into the shippable read-only form at stem.
func convertWorkingCopy(parent context.Context, r commandRunner, work, stem string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, convertTimeout)
	defer cancel()
	return convertToDMG(ctx, r, work, stem)
}

// verifyArtifact establishes that the staged file is a complete, readable disk
// image before anything renames it over the user's cartridge.
//
// The size check is not redundant with the checksum: a convert that produced no
// output at all leaves nothing for hdiutil to disagree with, and publishing an
// empty file over a working cartridge is the worst outcome this whole file
// exists to prevent.
func verifyArtifact(parent context.Context, r commandRunner, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat the new cartridge %s: %w", path, err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("the new cartridge %s is empty", path)
	}
	ctx, cancel := context.WithTimeout(parent, verifyTimeout)
	defer cancel()
	if _, errOut, runErr := r.run(ctx, hdiutil, verifyArgs(path)...); runErr != nil {
		return wrapHdiutil(cmdVerify, runErr, errOut)
	}
	return nil
}

// verifyArgs builds the `hdiutil verify` argument vector.
func verifyArgs(path string) []string {
	return []string{cmdVerify, path, flagQuiet}
}

// rescueWorkingCopy moves the guest's changes out of harm's way after a failed
// write-back and returns the error the user should see.
//
// Leaving the working copy where it is would not do: clearStaleWorkingCopy
// removes exactly that path on the next boot of the cartridge, which is right
// for a copy an earlier run abandoned and is silent data loss for one a failed
// write-back was trying to save. Under a rescue name it survives, and it is a
// runnable cartridge in its own right — `br boot <rescue>` picks up where the
// guest left off.
//
// The rename is best effort: if even that fails, say so, and say what the next
// boot will do to the file, so the user can move it aside by hand.
func (o *Opened) rescueWorkingCopy(cause error) error {
	work := o.WorkingCopy
	if work == "" {
		return cause
	}
	rescue := rescuePath(work, time.Now())
	if err := os.Rename(work, rescue); err != nil {
		return fmt.Errorf("%w; %s is unchanged, and the guest's changes are still in %s — move that file aside to keep them, because the next boot of the cartridge clears it: %w",
			cause, o.SourcePath, work, err)
	}
	// Cleared so no later step in Close can unlink what was just rescued.
	o.WorkingCopy = ""
	return fmt.Errorf("%w; %s is unchanged and the guest's changes were kept in %s — boot that file directly to recover them",
		cause, o.SourcePath, rescue)
}

// rescuePath names the image a failed write-back parks the guest's changes in:
// the working copy's own stem, a timestamp, and the runnable extension, so it
// can be booted as-is. A name already in use is suffixed rather than
// overwritten — the file it would replace is someone else's rescued work.
func rescuePath(work string, at time.Time) string {
	stem := TrimExt(work) + rescueInfix + at.Format(rescueTimeLayout)
	candidate := stem + SparseExt
	for n := 1; n <= maxRescueAttempts; n++ {
		if _, err := os.Stat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate
		}
		candidate = stem + "-" + strconv.Itoa(n) + SparseExt
	}
	return candidate
}
