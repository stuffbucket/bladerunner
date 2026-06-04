// Package cartridge builds and manages self-contained, AirDrop-able macOS disk
// images ("cartridges") that hold a complete bootable bladerunner VM: the disk
// manifest, the root disk, EFI + cloud-init state, and a read-write host<->guest
// share folder.
//
// A cartridge is backed by an APFS sparse image (.sparseimage, runnable) or a
// compressed read-only DMG (.dmg, the ship/AirDrop form). The public entry
// points are gated by platform: cartridge_darwin.go drives the real hdiutil via
// os/exec, while cartridge_other.go returns a clear "cartridges require macOS"
// error so the package builds cleanly on every host (CI is Linux).
//
// The heavy lifting (hdiutil argument construction, the busy->-force detach
// retry, output-path parsing, symlink-safe mountpoint comparison) lives here in
// platform-neutral code so it can be unit-tested without a real hdiutil. Those
// workers take a commandRunner, which tests replace with a fake.
package cartridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// File-name extensions for the two cartridge forms.
const (
	// SparseExt is the extension hdiutil gives a SPARSE/UDSP image.
	SparseExt = ".sparseimage"
	// DMGExt is the extension hdiutil gives a compressed UDZO image.
	DMGExt = ".dmg"
)

// Headroom sizing for a runnable cartridge. The sparse image is provisioned for
// the manifest disk size plus this slack so EFI vars, the cloud-init seed, the
// RW share folder, and filesystem overhead all fit. Sparse images only consume
// real bytes for real data, so over-provisioning is cheap.
const (
	// HeadroomGiB is the extra capacity (state + share + APFS slack) added on
	// top of the manifest disk size when sizing a cartridge.
	HeadroomGiB = 8
	// MinSizeGiB is the floor for a cartridge image so even a tiny disk leaves
	// room for the APFS container and the bladerunner payload.
	MinSizeGiB = HeadroomGiB + 2
)

// VolumePrefix is prepended to the cartridge name to form the APFS volume name
// passed to `hdiutil create -volname`.
const VolumePrefix = "bladerunner-"

// hdiutil is the macOS image tool. It is referenced as a plain string only; the
// binary itself exists solely on darwin, where the public wrappers run.
const hdiutil = "hdiutil"

// hdiutil image formats used by convert.
const (
	formatUDZO = "UDZO" // compressed, read-only (.dmg, the AirDrop form)
	formatUDSP = "UDSP" // read-write sparse (.sparseimage, runnable)
)

// Detach busy-retry tuning. hdiutil fails with exit 16 / "Resource busy" while a
// process still holds the volume; we retry a few times with backoff, then fall
// back to `hdiutil detach -force`.
const (
	detachRetries     = 3
	detachBackoff     = 500 * time.Millisecond
	createTimeout     = 5 * time.Minute
	attachTimeout     = 2 * time.Minute
	detachTimeout     = 2 * time.Minute
	compactTimeout    = 10 * time.Minute
	convertTimeout    = 30 * time.Minute
	mountpointDirPerm = 0o755
)

// ErrUnsupported is returned by every operation on non-darwin hosts. Cartridges
// rely on hdiutil and Apple's Virtualization.framework, which only exist on
// macOS.
var ErrUnsupported = errors.New("cartridges require macOS")

// Mount describes an attached cartridge image.
type Mount struct {
	// Path is the backing image file (.sparseimage or .dmg).
	Path string
	// Mountpoint is the resolved (symlink-evaluated) directory where the
	// cartridge volume is mounted.
	Mountpoint string
}

// commandRunner abstracts process execution so tests can inject a fake and run
// without a real hdiutil on the host. The production implementation
// (execRunner) shells out via exec.CommandContext.
type commandRunner interface {
	// run executes name with args and returns stdout and stderr separately so
	// callers can match on hdiutil's stderr messages, plus the exec error.
	run(ctx context.Context, name string, args ...string) (stdout string, stderr string, err error)
}

// execRunner is the production commandRunner backed by os/exec.
type execRunner struct{}

func (execRunner) run(ctx context.Context, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	return outBuf.String(), errBuf.String(), err
}

// defaultRunner is the runner used by the platform wrappers. Tests in this
// package may swap it to exercise the public API against a fake.
var defaultRunner commandRunner = execRunner{}

// SizeGiB computes the sparse-image capacity (in GiB) for a cartridge whose
// root disk is diskSizeGiB. It adds HeadroomGiB and clamps to MinSizeGiB so a
// zero/negative manifest size still yields a usable image.
func SizeGiB(diskSizeGiB int) int {
	size := diskSizeGiB + HeadroomGiB
	if size < MinSizeGiB {
		return MinSizeGiB
	}
	return size
}

// VolumeName returns the APFS volume name for a cartridge of the given name.
func VolumeName(name string) string {
	return VolumePrefix + name
}

// createArgs builds the `hdiutil create` argument vector for an APFS SPARSE
// cartridge. hdiutil auto-appends .sparseimage; we pass the full path (no
// double-append is observed) and confirm the real output path from stdout.
func createArgs(path, name string, sizeGiB int) []string {
	return []string{
		"create",
		"-type", "SPARSE",
		"-fs", "APFS",
		"-volname", VolumeName(name),
		"-size", fmt.Sprintf("%dg", sizeGiB),
		"-nospotlight",
		"-quiet",
		path,
	}
}

// attachArgs builds the `hdiutil attach` argument vector mounting at a known
// mountpoint, avoiding the need to parse a plist.
func attachArgs(path, mountpoint string) []string {
	return []string{
		"attach", path,
		"-mountpoint", mountpoint,
		"-nobrowse",
		"-owners", "on",
		"-noverify",
	}
}

// detachArgs builds the `hdiutil detach` argument vector for a mountpoint. When
// force is true the -force flag is appended.
func detachArgs(mountpoint string, force bool) []string {
	args := []string{"detach", mountpoint}
	if force {
		args = append(args, "-force")
	}
	return args
}

// convertArgs builds the `hdiutil convert` argument vector to format with the
// given destination stem. hdiutil appends the format extension.
func convertArgs(src, format, dst string) []string {
	return []string{"convert", src, "-format", format, "-o", dst, "-quiet"}
}

// compactArgs builds the `hdiutil compact` argument vector.
func compactArgs(path string) []string {
	return []string{"compact", path, "-quiet"}
}

// create provisions a new sparse cartridge image and returns the actual output
// path (resolved from hdiutil's "created:" line, falling back to the requested
// path with a guaranteed .sparseimage extension).
func create(ctx context.Context, r commandRunner, path, name string, sizeGiB int) (string, error) {
	out, errOut, err := r.run(ctx, hdiutil, createArgs(path, name, sizeGiB)...)
	if err != nil {
		return "", wrapHdiutil("create", err, errOut)
	}
	return resolveOutputPath(out, path, SparseExt), nil
}

// attach mounts the image at mountpoint (creating the dir first) and returns a
// Mount whose Mountpoint is symlink-resolved for reliable later comparison.
func attach(ctx context.Context, r commandRunner, path, mountpoint string) (*Mount, error) {
	if err := os.MkdirAll(mountpoint, mountpointDirPerm); err != nil {
		return nil, fmt.Errorf("create mountpoint %q: %w", mountpoint, err)
	}
	_, errOut, err := r.run(ctx, hdiutil, attachArgs(path, mountpoint)...)
	if err != nil {
		return nil, wrapHdiutil("attach", err, errOut)
	}
	return &Mount{Path: path, Mountpoint: resolvePath(mountpoint)}, nil
}

// detach unmounts the cartridge at mountpoint using the production backoff.
func detach(ctx context.Context, r commandRunner, mountpoint string) error {
	return detachWithBackoff(ctx, r, mountpoint, detachBackoff)
}

// detachWithBackoff unmounts the cartridge at mountpoint. It retries on
// "Resource busy" (exit 16) with the given backoff between attempts, then falls
// back to `detach -force`. A mountpoint that is already gone (No such file or
// directory) is treated as success. Tests pass backoff=0 to run fast.
func detachWithBackoff(ctx context.Context, r commandRunner, mountpoint string, backoff time.Duration) error {
	var lastErr error
	for attempt := 0; attempt <= detachRetries; attempt++ {
		_, errOut, err := r.run(ctx, hdiutil, detachArgs(mountpoint, false)...)
		if err == nil {
			return nil
		}
		if isAlreadyDetached(errOut) {
			return nil
		}
		lastErr = wrapHdiutil("detach", err, errOut)
		if !isBusy(errOut) {
			// A non-busy failure won't be cured by a force eject; surface it.
			return lastErr
		}
		if attempt < detachRetries && backoff > 0 {
			time.Sleep(backoff)
		}
	}

	// Busy after every retry: force the eject.
	_, errOut, err := r.run(ctx, hdiutil, detachArgs(mountpoint, true)...)
	if err == nil || isAlreadyDetached(errOut) {
		return nil
	}
	return fmt.Errorf("force %w", wrapHdiutil("detach", err, errOut))
}

// compact reclaims unused space in a (detached) sparse image.
func compact(ctx context.Context, r commandRunner, path string) error {
	_, errOut, err := r.run(ctx, hdiutil, compactArgs(path)...)
	if err != nil {
		return wrapHdiutil("compact", err, errOut)
	}
	return nil
}

// convertToDMG produces a compressed read-only DMG (the AirDrop artifact),
// returning the actual output path.
func convertToDMG(ctx context.Context, r commandRunner, src, dst string) (string, error) {
	return convert(ctx, r, src, formatUDZO, dst, DMGExt)
}

// convertToSparse produces a read-write sparse image (a runnable working copy
// from a shipped DMG), returning the actual output path.
func convertToSparse(ctx context.Context, r commandRunner, src, dst string) (string, error) {
	return convert(ctx, r, src, formatUDSP, dst, SparseExt)
}

func convert(ctx context.Context, r commandRunner, src, format, dst, wantExt string) (string, error) {
	out, errOut, err := r.run(ctx, hdiutil, convertArgs(src, format, dst)...)
	if err != nil {
		return "", wrapHdiutil("convert", err, errOut)
	}
	return resolveOutputPath(out, dst, wantExt), nil
}

// isAttached reports whether mountpoint currently has a mounted volume. It is
// symlink-safe and treats a missing directory as not attached.
func isAttached(mountpoint string) bool {
	resolved := resolvePath(mountpoint)
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return false
	}
	// A mounted APFS volume sits on a different device than its parent dir.
	return isMountpoint(resolved)
}

// wrapHdiutil decorates an exec error with the failing verb and hdiutil's
// stderr (trimmed) for an actionable message.
func wrapHdiutil(verb string, err error, stderr string) error {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return fmt.Errorf("hdiutil %s: %w", verb, err)
	}
	return fmt.Errorf("hdiutil %s: %w: %s", verb, err, stderr)
}

// isBusy reports whether hdiutil's stderr indicates the volume is still in use.
func isBusy(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "resource busy") || strings.Contains(s, "couldn't unmount")
}

// isAlreadyDetached reports whether hdiutil failed because nothing was attached
// at the path, which we treat as an idempotent success.
func isAlreadyDetached(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "no such file or directory") ||
		strings.Contains(s, "no such device") ||
		strings.Contains(s, "not currently mounted")
}

// resolveOutputPath extracts the path hdiutil reports on its "created: <path>"
// line. If absent, it falls back to the requested path, ensuring it carries the
// expected extension (hdiutil auto-appends it when missing).
func resolveOutputPath(stdout, requested, wantExt string) string {
	for line := range strings.SplitSeq(stdout, "\n") {
		line = strings.TrimSpace(line)
		const marker = "created:"
		if idx := strings.Index(strings.ToLower(line), marker); idx >= 0 {
			p := strings.TrimSpace(line[idx+len(marker):])
			if p != "" {
				return p
			}
		}
	}
	if !strings.HasSuffix(strings.ToLower(requested), wantExt) {
		return requested + wantExt
	}
	return requested
}

// resolvePath returns the symlink-resolved absolute form of p, falling back to
// the cleaned input when resolution fails (e.g. the path does not exist yet).
// macOS resolves /tmp -> /private/tmp, so comparisons must use this form.
func resolvePath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return filepath.Clean(p)
}

// The public API below is platform-neutral but only functional where
// hostSupported() reports true (darwin). On every other host each entry point
// returns ErrUnsupported, and hostSupported()/isMountpoint() are provided by the
// per-platform files so the workers above are referenced on all builds (no
// unused-code trap on Linux CI).

// Create provisions a new APFS SPARSE cartridge image of sizeGiB capacity at
// path (whose APFS volume is named bladerunner-<name>) and returns the actual
// output path hdiutil produced. Sparse images consume only real bytes, so the
// provisioned size is a ceiling, not a cost.
func Create(path, name string, sizeGiB int) (string, error) {
	if !hostSupported() {
		return "", ErrUnsupported
	}
	ctx, cancel := context.WithTimeout(context.Background(), createTimeout)
	defer cancel()
	return create(ctx, defaultRunner, path, name, sizeGiB)
}

// Attach mounts the cartridge image privately at mountpoint (-nobrowse) and
// returns a Mount with the symlink-resolved mountpoint.
func Attach(path, mountpoint string) (*Mount, error) {
	if !hostSupported() {
		return nil, ErrUnsupported
	}
	ctx, cancel := context.WithTimeout(context.Background(), attachTimeout)
	defer cancel()
	return attach(ctx, defaultRunner, path, mountpoint)
}

// Detach unmounts the cartridge at mountpoint, retrying on "Resource busy" and
// finally forcing the eject. An already-detached mountpoint is a no-op.
func Detach(mountpoint string) error {
	if !hostSupported() {
		return ErrUnsupported
	}
	ctx, cancel := context.WithTimeout(context.Background(), detachTimeout)
	defer cancel()
	return detach(ctx, defaultRunner, mountpoint)
}

// Compact reclaims unused space in a detached sparse cartridge image.
func Compact(path string) error {
	if !hostSupported() {
		return ErrUnsupported
	}
	ctx, cancel := context.WithTimeout(context.Background(), compactTimeout)
	defer cancel()
	return compact(ctx, defaultRunner, path)
}

// ConvertToDMG produces the compressed read-only AirDrop artifact from a sparse
// cartridge, returning the actual output path.
func ConvertToDMG(src, dst string) (string, error) {
	if !hostSupported() {
		return "", ErrUnsupported
	}
	ctx, cancel := context.WithTimeout(context.Background(), convertTimeout)
	defer cancel()
	return convertToDMG(ctx, defaultRunner, src, dst)
}

// ConvertToSparse produces a runnable read-write sparse working copy from a
// shipped DMG, returning the actual output path.
func ConvertToSparse(src, dst string) (string, error) {
	if !hostSupported() {
		return "", ErrUnsupported
	}
	ctx, cancel := context.WithTimeout(context.Background(), convertTimeout)
	defer cancel()
	return convertToSparse(ctx, defaultRunner, src, dst)
}

// IsAttached reports whether a cartridge volume is currently mounted at
// mountpoint. It is always false on unsupported hosts.
func IsAttached(mountpoint string) bool {
	if !hostSupported() {
		return false
	}
	return isAttached(mountpoint)
}
