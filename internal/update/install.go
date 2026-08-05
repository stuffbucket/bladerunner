package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/stuffbucket/bladerunner/internal/util"
)

// ErrHomebrewManaged is returned when the running binary lives inside a
// Homebrew prefix. Those installs are owned by `brew`; self-updating them would
// desync brew's bookkeeping, so we refuse and defer to `brew upgrade`.
var ErrHomebrewManaged = errors.New("update: this build is managed by Homebrew; run `brew upgrade bladerunner` instead")

// ErrNotAppBundle is returned when the running binary is neither Homebrew-
// managed nor inside a recognizable .app bundle, so there is no bundle to swap.
var ErrNotAppBundle = errors.New("update: running binary is not inside a Bladerunner.app bundle; reinstall from the .dmg to enable self-update")

// homebrewMarkers are path fragments that identify a Homebrew-managed binary.
// Both the Apple-silicon default prefix (/opt/homebrew) and the Intel/custom
// Cellar/linkage layout are covered.
var homebrewMarkers = []string{
	"/opt/homebrew/",
	"/homebrew/cellar/",
	"/homebrew/",
	"/.linuxbrew/",
}

// isHomebrewPath reports whether execPath sits under a Homebrew prefix. The
// check is case-insensitive because macOS filesystems are case-preserving but
// typically case-insensitive.
func isHomebrewPath(execPath string) bool {
	p := strings.ToLower(filepath.Clean(execPath))
	for _, m := range homebrewMarkers {
		if strings.Contains(p, m) {
			return true
		}
	}
	return false
}

// appBundleRoot walks up from the running executable to find the enclosing
// "*.app" bundle root (the directory ending in .app that contains
// Contents/MacOS/<binary>). It returns ErrNotAppBundle if none is found.
func appBundleRoot(execPath string) (string, error) {
	dir := filepath.Clean(execPath)
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ErrNotAppBundle
		}
		if strings.HasSuffix(strings.ToLower(dir), ".app") {
			return dir, nil
		}
		dir = parent
	}
}

// installTarget resolves where a self-update would be applied for the process
// whose executable is execPath. It fails closed for Homebrew installs and for
// binaries not inside an .app bundle, returning the bundle root to be swapped.
func installTarget(execPath string) (string, error) {
	if isHomebrewPath(execPath) {
		return "", ErrHomebrewManaged
	}
	return appBundleRoot(execPath)
}

// extractAppBundle unpacks a gzip tarball (as produced by macos-builder's
// updater artifact — a single top-level "<name>.app/…" tree) into destDir and
// returns the path to the extracted .app.
//
// Three refusals bound where the archive can write:
//
//  1. Lexical containment, delegated to util.SafeJoin, which owns that rule: an
//     entry name may not resolve outside destDir.
//  2. One bundle: every entry must sit inside the single top-level .app, so a
//     second bundle or a loose sibling file is refused instead of staged.
//  3. No symlink traversal: a symlink target must resolve inside the bundle, and
//     no component of an entry's path may already be a symlink on disk. Without
//     this, an entry that creates a link and a later entry that writes through it
//     escape the tree while every individual name still looks contained.
//
// Rules 2 and 3 live here, not in util.SafeJoin, because they are properties of
// untrusted archive extraction rather than of path joining. SafeJoin in
// particular is a pure lexical function, and its other caller
// (internal/imagebuild) writes into a mounted guest root whose own symlinks —
// /bin, /lib, /sbin — are legitimate paths to follow. Giving SafeJoin a
// no-follow rule would turn it into an IO function and would refuse those
// writes.
func extractAppBundle(tarball []byte, destDir string) (string, error) {
	gz, err := gzip.NewReader(bytes.NewReader(tarball))
	if err != nil {
		return "", fmt.Errorf("update: open gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var appRoot string
	cleanDest := filepath.Clean(destDir)

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("update: read tar: %w", err)
		}
		if !materializes(hdr.Typeflag) {
			// Devices, fifos, hard links and pax metadata headers create
			// nothing, so they need no destination and get no say in it.
			continue
		}

		// Guard against path traversal: the joined, cleaned path must stay
		// within destDir. internal/util owns the containment rule.
		target, err := util.SafeJoin(cleanDest, hdr.Name)
		if err != nil {
			return "", fmt.Errorf("update: tar entry escapes destination: %w", err)
		}
		if target == cleanDest {
			// The archive's own root entry ("./"). Nothing to create.
			continue
		}

		// Record the top-level .app directory to return.
		if appRoot == "" {
			if top := topAppComponent(hdr.Name); top != "" {
				appRoot = filepath.Join(cleanDest, top)
			}
		}
		if err := checkInBundle(cleanDest, appRoot, hdr.Name); err != nil {
			return "", err
		}

		if err := extractEntry(tr, hdr, cleanDest, appRoot, target); err != nil {
			return "", err
		}
	}

	if appRoot == "" {
		return "", fmt.Errorf("update: tarball did not contain a .app bundle")
	}
	return appRoot, nil
}

// materializes reports whether a tar type flag is one extraction writes to disk.
// A .app bundle is directories, regular files and symlinks; nothing else.
func materializes(typeflag byte) bool {
	switch typeflag {
	case tar.TypeDir, tar.TypeReg, tar.TypeSymlink:
		return true
	default:
		return false
	}
}

// checkInBundle refuses an entry that does not sit inside appRoot, the single
// top-level .app this extraction accepts.
func checkInBundle(cleanDest, appRoot, name string) error {
	top := topAppComponent(name)
	if top == "" {
		return fmt.Errorf("update: refusing tar entry %q: it is outside the app bundle", name)
	}
	if got := filepath.Join(cleanDest, top); got != appRoot {
		return fmt.Errorf("update: refusing tar entry %q: it belongs to a second bundle %q, not %q",
			name, top, filepath.Base(appRoot))
	}
	return nil
}

// extractEntry writes a single tar entry (dir, regular file, or in-tree
// symlink) to target. root is the extraction root the no-follow walk starts from
// and appRoot bounds where a symlink may point.
func extractEntry(tr *tar.Reader, hdr *tar.Header, root, appRoot, target string) error {
	if err := checkNoSymlinkComponent(root, target); err != nil {
		return fmt.Errorf("update: refusing tar entry %q: %w", hdr.Name, err)
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := os.MkdirAll(target, 0o755); err != nil {
			return fmt.Errorf("update: mkdir %q: %w", target, err)
		}
	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("update: mkdir parent of %q: %w", target, err)
		}
		return writeRegular(tr, target, os.FileMode(hdr.Mode))
	case tar.TypeSymlink:
		return extractSymlink(hdr, appRoot, target)
	}
	return nil
}

// checkNoSymlinkComponent walks target one component at a time from root and
// refuses if any component that already exists is a symlink. Entries are created
// in archive order, so the first component that does not exist ends the walk:
// nothing below it exists either.
func checkNoSymlinkComponent(root, target string) error {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return fmt.Errorf("resolve %q against %q: %w", target, root, err)
	}
	cur := root
	for part := range strings.SplitSeq(filepath.ToSlash(rel), "/") {
		cur = filepath.Join(cur, part)
		st, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect %q: %w", cur, err)
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%q is a symlink and extraction never writes through one", cur)
		}
	}
	return nil
}

// extractSymlink creates hdr's symlink at target, refusing a link whose target,
// resolved against the link's own parent directory, lands outside appRoot.
func extractSymlink(hdr *tar.Header, appRoot, target string) error {
	if filepath.IsAbs(hdr.Linkname) {
		return fmt.Errorf("update: refusing symlink %q: absolute target %q", hdr.Name, hdr.Linkname)
	}
	// The link is read from its own directory, so that is what the target is
	// relative to. Tar link targets are slash paths, so the two are resolved in
	// slash space and handed to SafeJoin — the owner of the containment rule —
	// which decides whether the result stays inside the bundle.
	dirRel, err := filepath.Rel(appRoot, filepath.Dir(target))
	if err != nil {
		return fmt.Errorf("update: refusing symlink %q: cannot place %q in the bundle: %w", hdr.Name, target, err)
	}
	resolved := path.Join(filepath.ToSlash(dirRel), filepath.ToSlash(hdr.Linkname))
	if _, err := util.SafeJoin(appRoot, filepath.FromSlash(resolved)); err != nil {
		return fmt.Errorf("update: refusing symlink %q: target %q resolves outside the app bundle: %w",
			hdr.Name, hdr.Linkname, err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("update: mkdir parent of symlink %q: %w", target, err)
	}
	if err := os.Symlink(hdr.Linkname, target); err != nil {
		return fmt.Errorf("update: symlink %q: %w", target, err)
	}
	return nil
}

// topAppComponent returns the first path component of name if it ends in
// ".app", else "".
func topAppComponent(name string) string {
	name = strings.TrimPrefix(filepath.ToSlash(name), "./")
	first, _, _ := strings.Cut(name, "/")
	if strings.HasSuffix(strings.ToLower(first), ".app") {
		return first
	}
	return ""
}

// writeRegular streams a tar entry to target with the given mode.
func writeRegular(r io.Reader, target string, mode os.FileMode) error {
	f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode.Perm())
	if err != nil {
		return fmt.Errorf("update: create %q: %w", target, err)
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return fmt.Errorf("update: write %q: %w", target, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("update: close %q: %w", target, err)
	}
	return nil
}

// swapBundle replaces the bundle at dst with newApp, which is already staged on
// the same filesystem so both moves are renames.
//
// The swap is two renames — dst to backup, then newApp to dst — and an
// interruption between them leaves the backup present and dst missing. That
// state is a recovery instruction, so each attempt reads the pair before it
// changes anything: a missing dst with a backup present is restored from the
// backup, and only a backup found beside an intact dst is a leftover safe to
// drop. dst is only ever created by a rename, so whenever it exists it is a
// complete generation; one complete generation therefore survives every point at
// which the process can stop. The parent directory is synced after each rename
// so the state a crash leaves on disk is the state this function reasons about.
//
// TestSwapBundle_RecoversFromInterruptedSwap and its neighbors in swap_test.go
// hold this contract.
func swapBundle(dst, newApp string) (err error) {
	parent := filepath.Dir(dst)
	backup := filepath.Join(parent, "."+filepath.Base(dst)+".old")

	if err := recoverInterruptedSwap(dst, backup, parent); err != nil {
		return err
	}

	movedAside := false
	if _, statErr := os.Lstat(dst); statErr == nil {
		if err := os.Rename(dst, backup); err != nil {
			return fmt.Errorf("update: move aside existing bundle: %w", err)
		}
		// Make the recovery state durable before opening the window in which a
		// crash leaves dst missing.
		util.SyncDir(parent)
		movedAside = true
	}

	// On failure after moving aside, restore the original.
	defer func() {
		if err != nil && movedAside {
			_ = os.RemoveAll(dst)
			_ = os.Rename(backup, dst)
			util.SyncDir(parent)
		}
	}()

	if err := os.Rename(newApp, dst); err != nil {
		return fmt.Errorf("update: install new bundle: %w", err)
	}
	// Commit: dst is durable before the backup goes away.
	util.SyncDir(parent)

	if movedAside {
		_ = os.RemoveAll(backup)
		util.SyncDir(parent)
	}
	return nil
}

// recoverInterruptedSwap reads the (dst, backup) pair and puts it back into a
// single-generation state before the caller changes anything.
//
// Three cases: no backup, nothing to do; a backup beside a missing dst, which is
// an interrupted swap and must be restored; a backup beside an intact dst, which
// is a leftover from a committed swap and is dropped.
func recoverInterruptedSwap(dst, backup, parent string) error {
	if _, err := os.Lstat(backup); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("update: refusing to swap: cannot inspect backup %q: %w", backup, err)
	}

	_, dstErr := os.Lstat(dst)
	switch {
	case dstErr == nil:
		// dst is intact, so the backup is a stale earlier generation.
		_ = os.RemoveAll(backup)
		util.SyncDir(parent)
		return nil
	case errors.Is(dstErr, os.ErrNotExist):
		if err := os.Rename(backup, dst); err != nil {
			return fmt.Errorf("update: refusing to swap: a previous swap was interrupted and "+
				"the backup %q could not be restored to %q: %w", backup, dst, err)
		}
		util.SyncDir(parent)
		return nil
	default:
		return fmt.Errorf("update: refusing to swap: cannot inspect %q: %w", dst, dstErr)
	}
}
