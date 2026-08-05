package vm

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/stuffbucket/bladerunner/internal/logging"
	"github.com/stuffbucket/bladerunner/internal/util"
)

// saveTransferTempPattern is the os.CreateTemp pattern appended to a
// destination file name to reserve its staging name. The per-destination prefix
// keeps two concurrent transfers apart, and the random suffix CreateTemp
// substitutes for the star makes the name unique.
const saveTransferTempPattern = ".move-*"

// linkFunc is the same-filesystem staging primitive: os.Link in production, and
// the seam a test uses to force the cross-filesystem branch.
type linkFunc func(oldname, newname string) error

// MoveSavedState moves a saved-state file and its metadata sidecar from src to
// dst as one generation, so a transfer cannot leave the two apart.
//
// The sidecar is what makes a saved state safe to restore: it records the disk
// image the RAM was frozen against, and a restore refuses a state file that
// arrives without one. A move that carried only the state file would therefore
// hand the operator a destination that looks like a snapshot and is not, which
// is why a source with no readable sidecar is refused outright rather than
// half-moved.
//
// The destination may live on another filesystem — `br save --path` accepts any
// path — so this does not rely on os.Rename, which fails with EXDEV across one.
// Both files are first staged into the destination's own directory, by hard
// link where the filesystem allows it (instant, whatever the size of the state
// file) and by copy-and-flush where it does not. Only when both are staged are
// they published, and only when both are published is the source removed.
//
// Failure before publication leaves the destination byte-for-byte untouched.
// Publication itself is two renames within one directory, and the destination's
// old sidecar is removed before them, so no moment exists in which the
// destination holds a state file beside a sidecar that describes a different
// one. If the second rename fails, the destination is emptied of the partial
// generation; the source generation is intact in every failure case, and the
// error says where it still is.
func MoveSavedState(src, dst string) error { return moveSavedState(src, dst, os.Link) }

// moveSavedState is MoveSavedState with the same-filesystem staging primitive
// injected, so a test can exercise the cross-filesystem (EXDEV) branch without
// mounting a second filesystem.
func moveSavedState(src, dst string, link linkFunc) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("saved state %s: %w", src, err)
	}
	// Identity, not spelling. A string compare misses every other way a path can
	// name this same file -- an extra slash, a relative form, a symlinked state
	// directory (on macOS /tmp and /var are symlinks, so this costs nothing to
	// hit). Falling through with an alias is unrecoverable rather than merely
	// wrong: the publish renames the staged link onto the source, and the
	// cleanup then unlinks what it just published, so the snapshot disappears
	// and the move still reports success.
	//
	// os.Rename, which this replaced, was a harmless no-op on an alias. Anything
	// weaker than SameFile here is a regression against it.
	if dstInfo, err := os.Stat(dst); err == nil && os.SameFile(srcInfo, dstInfo) {
		return nil
	}
	// Refuse a generation whose sidecar we cannot read. Loading it (rather than
	// stat-ing it) also rejects a truncated or unparseable sidecar, which is the
	// other way a destination ends up unrestorable.
	if _, err := LoadSaveMetadata(src); err != nil {
		return fmt.Errorf("refusing to move saved state %s: its metadata sidecar %s is missing or unreadable: %w", src, SaveMetadataPath(src), err)
	}

	stateTmp, err := stageForPublish(src, dst, link)
	if err != nil {
		return err
	}
	sidecarTmp, err := stageForPublish(SaveMetadataPath(src), SaveMetadataPath(dst), link)
	if err != nil {
		_ = os.Remove(stateTmp)
		return err
	}
	if err := publishMovedGeneration(dst, stateTmp, sidecarTmp); err != nil {
		return fmt.Errorf("%w (the saved state is unchanged at %s)", err, src)
	}

	// Both destination files are committed, so the source is now a duplicate.
	// Failing to remove it costs disk space, not correctness: what remains is a
	// complete, restorable generation, so the move is still reported as done.
	if err := removeSaveGeneration(src); err != nil {
		logging.L().Warn("saved state moved, but the source copy could not be removed", "src", src, "dst", dst, "err", err)
	}
	return nil
}

// publishMovedGeneration renames two staged files into place as one generation.
// The destination's old sidecar goes first: for the moment between the two
// renames the destination holds a state file with no sidecar, which a restore
// refuses, instead of a state file beside a sidecar that stamps a different
// disk, which a restore would believe.
func publishMovedGeneration(dst, stateTmp, sidecarTmp string) error {
	if err := os.Remove(SaveMetadataPath(dst)); err != nil && !os.IsNotExist(err) {
		removeStaged(stateTmp, sidecarTmp)
		return fmt.Errorf("remove stale metadata at destination %s: %w", SaveMetadataPath(dst), err)
	}
	if err := util.PublishFileAtomic(stateTmp, dst); err != nil {
		removeStaged(stateTmp, sidecarTmp)
		return fmt.Errorf("publish saved state at %s: %w", dst, err)
	}
	if err := util.PublishFileAtomic(sidecarTmp, SaveMetadataPath(dst)); err != nil {
		// The state landed and its sidecar did not. Take the state back out:
		// alone it is refused by any restore anyway, and left in place it invites
		// an operator to treat the destination as a usable snapshot.
		_ = os.Remove(dst)
		removeStaged(stateTmp, sidecarTmp)
		return fmt.Errorf("publish saved-state metadata at %s: %w", SaveMetadataPath(dst), err)
	}
	return nil
}

// removeStaged discards staging files. They are copies or hard links, never the
// only copy: the source generation stays in place until both destination files
// are committed.
func removeStaged(paths ...string) {
	for _, p := range paths {
		_ = os.Remove(p)
	}
}

// stageForPublish places the contents of src at a uniquely named file in dst's
// own directory and returns that path. Nothing at dst is touched, and src is
// left in place, so a caller can abandon the transfer by deleting what this
// returned.
func stageForPublish(src, dst string, link linkFunc) (string, error) {
	tmp, err := reserveTempName(filepath.Dir(dst), filepath.Base(dst))
	if err != nil {
		return "", err
	}
	// Same filesystem: a hard link is instant however large the state file is,
	// and it does not consume the source.
	if err := link(src, tmp); err == nil {
		return tmp, nil
	}
	// Cross-filesystem (EXDEV), or a filesystem with no hard links at all: copy
	// the bytes and flush them, so the publish below only has to rename. The
	// copy is owner-only, like the sidecar: a saved state is the RAM of a guest.
	if err := util.CopyFileDurable(src, tmp, saveGenerationMode); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("stage %s for %s: %w", src, dst, err)
	}
	return tmp, nil
}

// reserveTempName returns an unused file name in dir, derived from base. The
// name is reserved by creating the file and removing it again, because os.Link
// refuses a name that exists but os.CreateTemp is the only way to get a name
// nothing else in this process will pick.
func reserveTempName(dir, base string) (string, error) {
	f, err := os.CreateTemp(dir, base+saveTransferTempPattern)
	if err != nil {
		return "", fmt.Errorf("create staging file in %s: %w", dir, err)
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("close staging file %s: %w", name, err)
	}
	if err := os.Remove(name); err != nil {
		return "", fmt.Errorf("clear staging file %s: %w", name, err)
	}
	return name, nil
}
