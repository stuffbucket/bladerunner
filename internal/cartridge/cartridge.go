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
	"encoding/xml"
	"errors"
	"fmt"
	"io"
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

// hdiutil subcommands and flags referenced in more than one place (arg vectors
// and error labels), hoisted to constants.
const (
	cmdCreate  = "create"
	cmdAttach  = "attach"
	cmdDetach  = "detach"
	cmdConvert = "convert"
	cmdCompact = "compact"
	flagFormat = "-format"
	flagQuiet  = "-quiet"
	flagForce  = "-force"
	flagPlist  = "-plist"
)

// devNodePrefix is the prefix every BSD disk device node carries. An attached
// disk image is always backed by /dev/diskN[sM]; a synthetic mount (autofs,
// devfs, a firmlink) is not, which is what makes this a usable identity test.
const devNodePrefix = "/dev/disk"

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
	// DevNode is the BSD device node of the mounted volume (e.g.
	// /dev/disk4s1), captured from `hdiutil attach -plist`. It is the handle
	// DiskArbitration and `diskutil` address a volume by, so cartridge
	// eject/unmount-request handling needs it. Best-effort: empty when hdiutil
	// emitted no parseable plist and the kernel could not be asked either.
	DevNode string
	// Policy is the mount policy the volume was attached under. It is what
	// tells a holder whether the user can Finder-eject this cartridge (and so
	// whether an unmount-approval veto is worth registering) or whether the
	// mount is private and can only go away because we said so.
	Policy MountPolicy
}

// MountInfo is the kernel's own view of a mounted volume, obtained from statfs.
// It is the authoritative answer to "what is actually mounted here?", unlike a
// st.Dev comparison which merely says "this differs from its parent".
type MountInfo struct {
	// Mountpoint is the mount root the kernel reports (f_mntonname). It equals
	// the queried path only when that path IS the root of a mount.
	Mountpoint string
	// DevNode is the backing device (f_mntfromname), e.g. /dev/disk4s1.
	DevNode string
	// FSType is the filesystem name (f_fstypename), e.g. "apfs".
	FSType string
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
//
// The volume name is DERIVED from path rather than accepted as a parameter, and
// that is the whole point: a cartridge's identity is its own file name, so the
// name baked into the volume cannot disagree with the name NameFromPath gives
// the same file. See Create for what that disagreement used to cost.
func createArgs(path string, sizeGiB int) []string {
	return []string{
		cmdCreate,
		"-type", "SPARSE",
		"-fs", "APFS",
		"-volname", VolumeName(NameFromPath(path)),
		"-size", fmt.Sprintf("%dg", sizeGiB),
		"-nospotlight",
		flagQuiet,
		path,
	}
}

// detachArgs builds the `hdiutil detach` argument vector for a mountpoint. When
// force is true the -force flag is appended.
func detachArgs(mountpoint string, force bool) []string {
	args := []string{cmdDetach, mountpoint}
	if force {
		args = append(args, flagForce)
	}
	return args
}

// convertArgs builds the `hdiutil convert` argument vector to format with the
// given destination stem. hdiutil appends the format extension.
func convertArgs(src, format, dst string) []string {
	return []string{cmdConvert, src, flagFormat, format, "-o", dst, flagQuiet}
}

// compactArgs builds the `hdiutil compact` argument vector.
func compactArgs(path string) []string {
	return []string{cmdCompact, path, flagQuiet}
}

// create provisions a new sparse cartridge image and returns the actual output
// path (resolved from hdiutil's "created:" line, falling back to the requested
// path with a guaranteed .sparseimage extension).
func create(ctx context.Context, r commandRunner, path string, sizeGiB int) (string, error) {
	out, errOut, err := r.run(ctx, hdiutil, createArgs(path, sizeGiB)...)
	if err != nil {
		return "", wrapHdiutil(cmdCreate, err, errOut)
	}
	return resolveOutputPath(out, path, SparseExt), nil
}

// attach mounts the image according to req's policy and returns the resulting
// Mount. Under MountPrivate the mountpoint is dictated by the caller; under
// MountBrowsable macOS chooses and the real location is read back out of
// hdiutil's plist (see mountpolicy.go).
func attach(ctx context.Context, r commandRunner, req attachRequest) (*Mount, error) {
	if req.policy.Private() {
		return attachPrivate(ctx, r, req)
	}
	return attachBrowsable(ctx, r, req)
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
		lastErr = wrapHdiutil(cmdDetach, err, errOut)
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
	return fmt.Errorf("force %w", wrapHdiutil(cmdDetach, err, errOut))
}

// compact reclaims unused space in a (detached) sparse image.
func compact(ctx context.Context, r commandRunner, path string) error {
	_, errOut, err := r.run(ctx, hdiutil, compactArgs(path)...)
	if err != nil {
		return wrapHdiutil(cmdCompact, err, errOut)
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
		return "", wrapHdiutil(cmdConvert, err, errOut)
	}
	return resolveOutputPath(out, dst, wantExt), nil
}

// isAttached reports whether a real disk-image volume is mounted at mountpoint.
//
// The st.Dev comparison in isMountpoint is only a cheap first filter: a device
// id that differs from the parent's is also true of firmlinks and of any
// unrelated volume that happens to be mounted there, so on its own it cannot
// distinguish a cartridge from, say, a stray USB stick or an autofs stub. We
// therefore ask the kernel what is actually mounted and require both that it
// agrees mountpoint IS the mount root (f_mntonname round-trips) and that the
// volume is backed by a /dev/disk* node, which is what an attached image is.
func isAttached(mountpoint string) bool {
	resolved := resolvePath(mountpoint)
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return false
	}
	// Fast path: a mounted volume sits on a different device than its parent.
	if !isMountpoint(resolved) {
		return false
	}
	mi, err := lookupMount(resolved)
	if err != nil {
		return false
	}
	return mi.Mountpoint == resolved && strings.HasPrefix(mi.DevNode, devNodePrefix)
}

// isAttachedFrom reports whether mountpoint is the mount root of the volume
// backed by exactly devNode — the strongest identity check available, used when
// the caller already holds the Mount it created.
func isAttachedFrom(mountpoint, devNode string) bool {
	resolved := resolvePath(mountpoint)
	mi, err := lookupMount(resolved)
	if err != nil {
		return false
	}
	return mi.Mountpoint == resolved && mi.DevNode == devNode
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

// --- hdiutil attach -plist parsing ---------------------------------------
//
// `hdiutil attach -plist` emits an Apple property list whose "system-entities"
// array holds one dict per device node the image produced. For an APFS sparse
// cartridge that is typically four entries: the GUID partition scheme, the
// Apple_APFS container, the synthesized APFS volume group, and the single
// mounted volume — only the last of which carries a "mount-point". We want that
// entry's "dev-entry".
//
// No plist library is a dependency of this module and we will not add one for
// two keys, so the decoder below handles exactly the plist subset hdiutil emits
// (dict/array/string/integer/data/true/false) on top of encoding/xml.

// plist keys consumed from a system-entities entry.
const (
	plistKeyEntities   = "system-entities"
	plistKeyDevEntry   = "dev-entry"
	plistKeyMountPoint = "mount-point"
	plistKeyVolumeKind = "volume-kind"
)

// plist element names the decoder distinguishes; everything else is treated as
// a scalar whose character data is the value.
const (
	plistElemDict  = "dict"
	plistElemArray = "array"
	plistElemKey   = "key"
	plistElemTrue  = "true"
	plistElemFalse = "false"
)

// errNoPlistRoot is returned when the input carries no top-level plist dict —
// e.g. hdiutil was run without -plist, or printed a bare status line.
var errNoPlistRoot = errors.New("no plist dictionary in output")

// systemEntity is one device node reported by `hdiutil attach -plist`. Entities
// that are not mountable (the partition scheme, the APFS container) have an
// empty MountPoint.
type systemEntity struct {
	// DevEntry is the BSD device node, e.g. /dev/disk4s1.
	DevEntry string
	// MountPoint is where hdiutil mounted this entity, or "" if unmounted.
	MountPoint string
	// VolumeKind is the filesystem kind, e.g. "apfs"; "" when not a volume.
	VolumeKind string
}

// attachedDevNode returns the BSD device node of the volume hdiutil mounted at
// mountpoint, or "" when the output carried no usable plist. It never errors:
// the device node is additive information, and a cartridge that mounted fine
// must not be rejected because its plist was unexpected.
func attachedDevNode(stdout, mountpoint string) string {
	entities, err := parseAttachEntities(stdout)
	if err != nil {
		return ""
	}
	return selectMountedDevNode(entities, mountpoint)
}

// selectMountedDevNode picks the entity mounted at mountpoint, comparing both
// the literal and the symlink-resolved form (hdiutil reports /private/tmp/x for
// a /tmp/x request). When no entity matches — an hdiutil that mounted somewhere
// unexpected — it falls back to the first entity that is mounted at all, since
// a single-volume cartridge has exactly one.
func selectMountedDevNode(entities []systemEntity, mountpoint string) string {
	want := resolvePath(mountpoint)
	fallback := ""
	for _, e := range entities {
		if e.DevEntry == "" || e.MountPoint == "" {
			continue
		}
		if e.MountPoint == mountpoint || resolvePath(e.MountPoint) == want {
			return e.DevEntry
		}
		if fallback == "" {
			fallback = e.DevEntry
		}
	}
	return fallback
}

// parseAttachEntities decodes the system-entities array from `hdiutil attach
// -plist` stdout.
func parseAttachEntities(stdout string) ([]systemEntity, error) {
	root, err := parsePlistRootDict(stdout)
	if err != nil {
		return nil, err
	}
	raw, ok := root[plistKeyEntities].([]any)
	if !ok {
		return nil, fmt.Errorf("plist has no %s array: %w", plistKeyEntities, errNoPlistRoot)
	}
	entities := make([]systemEntity, 0, len(raw))
	for _, item := range raw {
		dict, ok := item.(map[string]any)
		if !ok {
			continue
		}
		entities = append(entities, systemEntity{
			DevEntry:   plistString(dict, plistKeyDevEntry),
			MountPoint: plistString(dict, plistKeyMountPoint),
			VolumeKind: plistString(dict, plistKeyVolumeKind),
		})
	}
	return entities, nil
}

// plistString reads a string-valued key from a decoded plist dict, yielding ""
// for a missing key or a non-string value.
func plistString(dict map[string]any, key string) string {
	s, _ := dict[key].(string)
	return s
}

// parsePlistRootDict decodes the outermost <dict> of a property list.
func parsePlistRootDict(s string) (map[string]any, error) {
	dec := xml.NewDecoder(strings.NewReader(s))
	// hdiutil emits a DOCTYPE referencing Apple's external DTD; never fetch it.
	dec.Entity = xml.HTMLEntity
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil, errNoPlistRoot
		}
		if err != nil {
			return nil, fmt.Errorf("parse plist: %w", err)
		}
		if start, ok := tok.(xml.StartElement); ok && start.Name.Local == plistElemDict {
			return decodePlistDict(dec)
		}
	}
}

// decodePlistDict consumes tokens up to the </dict> that closes an already-read
// <dict>, pairing each <key> with the value element that follows it.
func decodePlistDict(dec *xml.Decoder) (map[string]any, error) {
	out := map[string]any{}
	pendingKey := ""
	haveKey := false
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("parse plist dict: %w", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			// The only EndElement reachable here closes this dict: </key> is
			// consumed by DecodeElement and nested values by decodePlistValue.
			if _, isEnd := tok.(xml.EndElement); isEnd {
				return out, nil
			}
			continue
		}
		if start.Name.Local == plistElemKey {
			if err := dec.DecodeElement(&pendingKey, &start); err != nil {
				return nil, fmt.Errorf("parse plist key: %w", err)
			}
			haveKey = true
			continue
		}
		value, err := decodePlistValue(dec, start)
		if err != nil {
			return nil, err
		}
		if haveKey {
			out[pendingKey] = value
			haveKey = false
		}
	}
}

// decodePlistArray consumes tokens up to the </array> that closes an
// already-read <array>.
func decodePlistArray(dec *xml.Decoder) ([]any, error) {
	var out []any
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("parse plist array: %w", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			if _, isEnd := tok.(xml.EndElement); isEnd {
				return out, nil
			}
			continue
		}
		value, err := decodePlistValue(dec, start)
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
}

// decodePlistValue decodes the value element that start opens, leaving the
// decoder positioned just past that element's closing tag.
func decodePlistValue(dec *xml.Decoder, start xml.StartElement) (any, error) {
	switch start.Name.Local {
	case plistElemDict:
		return decodePlistDict(dec)
	case plistElemArray:
		return decodePlistArray(dec)
	case plistElemTrue, plistElemFalse:
		if err := dec.Skip(); err != nil {
			return nil, fmt.Errorf("parse plist bool: %w", err)
		}
		return start.Name.Local == plistElemTrue, nil
	default:
		// string, integer, real, date, data: character data is the value.
		var s string
		if err := dec.DecodeElement(&s, &start); err != nil {
			return nil, fmt.Errorf("parse plist %s: %w", start.Name.Local, err)
		}
		return s, nil
	}
}

// The public API below is platform-neutral but only functional where
// hostSupported() reports true (darwin). On every other host each entry point
// returns ErrUnsupported, and hostSupported()/isMountpoint() are provided by the
// per-platform files so the workers above are referenced on all builds (no
// unused-code trap on Linux CI).

// Create provisions a new APFS SPARSE cartridge image of sizeGiB capacity at
// path and returns the actual output path hdiutil produced. Sparse images
// consume only real bytes, so the provisioned size is a ceiling, not a cost.
//
// The volume is named bladerunner-<NameFromPath(path)> — after the CARTRIDGE
// being written, never after whatever disk supplied its contents. Three names
// have to agree for a cartridge to work, and this is where all three are
// seeded: the volume mount detection reads back (NameFromVolume), the name `br
// boot <file>` derives (NameFromPath), and the instance name the boot then
// registers under. Taking the name as a parameter is what let `br disk pack
// <disk> --out <other>.sparseimage` bake the DISK's name into the volume: every
// cartridge packed from one base disk then mounted at the same /Volumes path,
// and `br watch` reported a name the user could not eject by.
//
// A caller passing a user-supplied path must check the derived name itself
// (instance.ValidName): Create is a plain hdiutil wrapper and does not judge it.
func Create(path string, sizeGiB int) (string, error) {
	if !hostSupported() {
		return "", ErrUnsupported
	}
	ctx, cancel := context.WithTimeout(context.Background(), createTimeout)
	defer cancel()
	return create(ctx, defaultRunner, path, sizeGiB)
}

// Attach mounts the cartridge image privately at mountpoint (-nobrowse) and
// returns a Mount with the symlink-resolved mountpoint.
//
// This entry point is deliberately pinned to MountPrivate: it serves the
// build-side flows (`br disk pack`, layout inspection) that need a
// deterministic location and must never contend with a booted cartridge, which
// lands under /Volumes. Use Open with a MountPolicy to attach a cartridge for
// booting.
func Attach(path, mountpoint string) (*Mount, error) {
	if !hostSupported() {
		return nil, ErrUnsupported
	}
	ctx, cancel := context.WithTimeout(context.Background(), attachTimeout)
	defer cancel()
	return attach(ctx, defaultRunner, attachRequest{
		path:       path,
		mountpoint: mountpoint,
		policy:     MountPrivate,
	})
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
//
// This is an identity check, not a "something is here" check: the mountpoint
// must be the mount root the kernel reports and must be backed by a /dev/disk*
// device. Use IsAttachedFrom when the caller holds the Mount and can assert the
// exact device, and Verify to additionally require a coherent cartridge layout.
func IsAttached(mountpoint string) bool {
	if !hostSupported() {
		return false
	}
	return isAttached(mountpoint)
}

// IsAttachedFrom reports whether mountpoint is the mount root of the volume
// backed by devNode (as captured in Mount.DevNode). This is the precise check
// for "is MY cartridge still mounted where I put it" — it cannot be fooled by
// an unrelated volume that was mounted over the same path. An empty devNode
// is always false, since it asserts nothing.
func IsAttachedFrom(mountpoint, devNode string) bool {
	if !hostSupported() || devNode == "" {
		return false
	}
	return isAttachedFrom(mountpoint, devNode)
}

// LookupMount returns the kernel's view of the volume mounted at mountpoint:
// its true mount root, backing BSD device node, and filesystem type. It is the
// bridge to DiskArbitration, which addresses volumes by BSD name rather than by
// path. An error means nothing is mounted there (or the path is unreadable).
func LookupMount(mountpoint string) (*MountInfo, error) {
	if !hostSupported() {
		return nil, ErrUnsupported
	}
	info, err := lookupMount(resolvePath(mountpoint))
	if err != nil {
		return nil, err
	}
	return &info, nil
}

// DevNodeAt returns the BSD device node (e.g. /dev/disk4s1) backing the volume
// mounted at mountpoint. Use it to recover a device node for a cartridge that
// was attached by an earlier process, where no Mount value survives.
func DevNodeAt(mountpoint string) (string, error) {
	info, err := LookupMount(mountpoint)
	if err != nil {
		return "", err
	}
	return info.DevNode, nil
}
