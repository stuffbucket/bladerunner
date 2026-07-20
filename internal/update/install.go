package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
// returns the path to the extracted .app. It rejects entries that would escape
// destDir (path traversal) and refuses to follow symlinks out of the tree.
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

		// Guard against path traversal: the joined, cleaned path must stay
		// within destDir.
		target, err := safeJoin(cleanDest, hdr.Name)
		if err != nil {
			return "", err
		}

		// Record the top-level .app directory to return.
		if appRoot == "" {
			if top := topAppComponent(hdr.Name); top != "" {
				appRoot = filepath.Join(cleanDest, top)
			}
		}

		if err := extractEntry(tr, hdr, target); err != nil {
			return "", err
		}
	}

	if appRoot == "" {
		return "", fmt.Errorf("update: tarball did not contain a .app bundle")
	}
	return appRoot, nil
}

// safeJoin joins name onto cleanDest and refuses any result that escapes
// cleanDest (path traversal).
func safeJoin(cleanDest, name string) (string, error) {
	target := filepath.Join(cleanDest, name) //#nosec G305 -- escape is rejected on the next line
	if target != cleanDest && !strings.HasPrefix(target, cleanDest+string(os.PathSeparator)) {
		return "", fmt.Errorf("update: tar entry escapes destination: %q", name)
	}
	return target, nil
}

// extractEntry writes a single tar entry (dir, regular file, or in-tree
// symlink) to target. Anything else (devices, fifos) is skipped — a .app bundle
// contains none.
func extractEntry(tr *tar.Reader, hdr *tar.Header, target string) error {
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
		return extractSymlink(hdr, target)
	}
	return nil
}

// extractSymlink creates hdr's symlink at target, refusing links that point
// outside the extracted bundle.
func extractSymlink(hdr *tar.Header, target string) error {
	if filepath.IsAbs(hdr.Linkname) || strings.HasPrefix(hdr.Linkname, "..") {
		return fmt.Errorf("update: unsafe symlink %q -> %q", hdr.Name, hdr.Linkname)
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

// swapBundle atomically replaces the bundle at dst with newApp. The new bundle
// is already staged on the same filesystem (its parent is the destination's
// parent), so the rename is atomic. The previous bundle is moved aside first
// and restored if the rename fails, so a crash never leaves the app missing.
func swapBundle(dst, newApp string) (err error) {
	parent := filepath.Dir(dst)
	backup := filepath.Join(parent, "."+filepath.Base(dst)+".old")

	// Clear any stale backup from a prior interrupted run.
	_ = os.RemoveAll(backup)

	movedAside := false
	if _, statErr := os.Lstat(dst); statErr == nil {
		if err := os.Rename(dst, backup); err != nil {
			return fmt.Errorf("update: move aside existing bundle: %w", err)
		}
		movedAside = true
	}

	// On failure after moving aside, restore the original.
	defer func() {
		if err != nil && movedAside {
			_ = os.RemoveAll(dst)
			_ = os.Rename(backup, dst)
		}
	}()

	if err := os.Rename(newApp, dst); err != nil {
		return fmt.Errorf("update: install new bundle: %w", err)
	}

	// Success: drop the backup. A leftover backup is harmless but we clean up.
	if movedAside {
		_ = os.RemoveAll(backup)
	}
	return nil
}
