package vm

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/httpfetch"
	"github.com/stuffbucket/bladerunner/internal/logging"
	"github.com/stuffbucket/bladerunner/internal/util"
)

// Network budgets for the two guest-image fetches. They are package vars, not
// constants, only so a test can shorten them: a test that had to wait out the
// production budget would either be skipped or would itself be the hang it is
// meant to catch.
var (
	// sidecarTimeout is a FLAT deadline on the whole sidecar exchange. The
	// sidecar is a single 64-character digest, it is fetched before anything
	// else on a cold boot, and a stalled one hangs the boot with no output at
	// all — so total duration is the right bound and it can be tight.
	sidecarTimeout = 30 * time.Second

	// downloadStallTimeout bounds time WITHOUT PROGRESS on a base image, which
	// is roughly a gigabyte. A flat deadline is the wrong tool here: it would
	// cap total transfer time and break an honest download over a slow link,
	// while still letting a wedged peer trickle bytes underneath it forever.
	downloadStallTimeout = httpfetch.StallTimeout
)

// fetchSidecarSHA256 fetches a "<url>.sha256" sidecar and returns the
// lowercased hex digest. The sidecar may be either bare hex or the
// `sha256sum` format ("<hex>  <filename>"); only the first whitespace-
// separated token is used. Returns "" with no error if the sidecar
// 404s (caller decides whether that's acceptable).
func fetchSidecarSHA256(ctx context.Context, imageURL string) (string, error) {
	sidecarURL := imageURL + ".sha256"
	// No stall watchdog: the flat client deadline already covers this body.
	resp, err := httpfetch.Get(ctx, httpfetch.Client(sidecarTimeout), sidecarURL, 0)
	if err != nil {
		return "", fmt.Errorf("fetch sidecar checksum: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("fetch sidecar checksum: %s", resp.Status)
	}

	const maxSidecarBytes = 4096
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxSidecarBytes))
	if err != nil {
		return "", fmt.Errorf("read sidecar checksum: %w", err)
	}
	first := strings.Fields(strings.TrimSpace(string(b)))
	if len(first) == 0 {
		return "", fmt.Errorf("sidecar checksum is empty")
	}
	digest := strings.ToLower(first[0])
	if len(digest) != sha256.Size*2 {
		return "", fmt.Errorf("sidecar checksum has unexpected length: %d", len(digest))
	}
	for _, r := range digest {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return "", fmt.Errorf("sidecar checksum is not hex: %q", digest)
		}
	}
	return digest, nil
}

// fileSHA256 returns the hex-encoded SHA-256 digest of the file at path.
func fileSHA512(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open for sha512: %w", err)
	}
	defer func() { _ = f.Close() }()

	h := sha512.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash file: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// verifyImageChecksum verifies the downloaded image at path. When expectedSHA512
// is non-empty (the pinned Debian default), it is checked against that embedded
// hash and a mismatch is fatal — this makes the default image reproducible and
// tamper-evident without a network round-trip.
//
// Otherwise verification falls back to a sidecar checksum hosted at
// imageURL+".sha256". The strictSidecar flag decides how a missing/unreachable
// sidecar is treated:
//
//   - strictSidecar=true (the pre-baked hosted guest image, which always ships a
//     published .sha256): FAIL CLOSED. A mismatch, a missing/404 sidecar, or an
//     unreachable sidecar host are all fatal — parity with the pinned-Debian
//     SHA-512 path. The hosted image is release-managed, so its checksum must
//     always be present and correct; a gap means "do not boot", not "boot
//     anyway".
//   - strictSidecar=false (a user-supplied --image-url): a missing or unreachable
//     sidecar is logged at WARN and skipped — many upstream image hosts
//     (cloud.debian.org, arbitrary URLs) don't publish per-image .sha256
//     sidecars, and blocking boot on their absence regresses that experience.
//     A mismatched sidecar remains fatal in both modes.
func verifyImageChecksum(ctx context.Context, imageURL, expectedSHA512 string, strictSidecar bool, path string) error {
	if expectedSHA512 != "" {
		got, err := fileSHA512(path)
		if err != nil {
			return err
		}
		if !strings.EqualFold(got, expectedSHA512) {
			return fmt.Errorf("base image SHA-512 mismatch: got %s, want %s", got, expectedSHA512)
		}
		logging.L().Info("base image SHA-512 verified (pinned)", "sha512", got)
		return nil
	}

	want, err := fetchSidecarSHA256(ctx, imageURL)
	if err != nil {
		if strictSidecar {
			return fmt.Errorf("hosted image sidecar SHA-256 unreachable (%s): %w", imageURL+".sha256", err)
		}
		logging.L().Warn("sidecar SHA-256 fetch failed, continuing without verification",
			"url", imageURL+".sha256", "err", err)
		return nil
	}
	if want == "" {
		if strictSidecar {
			return fmt.Errorf("hosted image sidecar SHA-256 missing (%s): refusing to boot unverified", imageURL+".sha256")
		}
		logging.L().Warn("sidecar SHA-256 not present, skipping verification",
			"url", imageURL+".sha256")
		return nil
	}
	got, err := util.FileSHA256(path)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("base image SHA-256 mismatch: got %s, want %s", got, want)
	}
	logging.L().Info("base image SHA-256 verified", "sha256", got)
	return nil
}

// EnsureBaseImage resolves cfg's image source to a local RAW disk image,
// downloading and caching/converting as needed, and returns its path. It is the
// exported entry point used by `br disk pack` to materialize a cartridge's
// root.img without starting a VM; it shares the exact same cache/convert path as
// boot, so a disk packed and a disk booted resolve the identical bytes.
func EnsureBaseImage(ctx context.Context, cfg *config.Config) (string, error) {
	return ensureBaseImage(ctx, cfg)
}

// RequireQemuImg verifies that the qemu-img binary is available on PATH,
// returning an install-hint error if it is not. It is the single preflight
// check for the qemu-img dependency shared across asset conversion/resize and
// the `br disk build` command.
func RequireQemuImg() error {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		return fmt.Errorf("qemu-img not found in PATH (install with: brew install qemu): %w", err)
	}
	return nil
}

// MaterializeRawDisk copies a resolved RAW base image to dst and resizes it to
// diskSizeGiB via qemu-img (which correctly rewrites the GPT backup header).
// Used by `br disk pack` to write the cartridge's root.img. srcRaw must
// already be raw (EnsureBaseImage guarantees this).
func MaterializeRawDisk(srcRaw, dst string, diskSizeGiB int) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create root.img parent: %w", err)
	}
	in, err := os.Open(srcRaw)
	if err != nil {
		return fmt.Errorf("open base image: %w", err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create root.img: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy base image into cartridge: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close root.img: %w", err)
	}

	if err := RequireQemuImg(); err != nil {
		return err
	}
	targetSize := fmt.Sprintf("%dG", diskSizeGiB)
	cmd := exec.Command("qemu-img", "resize", "-f", "raw", dst, targetSize)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img resize failed: %w: %s", err, string(output))
	}
	return nil
}

func ensureBaseImage(ctx context.Context, cfg *config.Config) (string, error) {
	if cfg.BaseImagePath != "" {
		if !util.FileExists(cfg.BaseImagePath) {
			return "", fmt.Errorf("base image path does not exist: %s", cfg.BaseImagePath)
		}
		if err := ensureRawDiskImage(ctx, cfg.BaseImagePath); err != nil {
			return "", err
		}
		logging.L().Info("using provided base image", "path", cfg.BaseImagePath)
		return cfg.BaseImagePath, nil
	}

	// When a disk manifest pins an explicit SHA-256 of the downloaded artifact,
	// materialize the base image once into the shared content-addressed cache and
	// reuse it across every disk slot.
	if cfg.BaseImageExpectedSHA256 != "" {
		return ensureCachedBaseImage(ctx, cfg, cfg.BaseImageExpectedSHA256)
	}

	path := filepath.Join(cfg.VMDir, "base-image.raw")
	if util.FileExists(path) {
		if err := ensureRawDiskImage(ctx, path); err != nil {
			return "", err
		}
		logging.L().Info("using cached base image", "path", path)
		return path, nil
	}

	if cfg.BaseImageURL == "" {
		return "", fmt.Errorf("base image url is empty")
	}

	// The default is the pre-baked hosted image, verified fail-closed against its
	// .sha256 sidecar. If it can't be used — 404/missing asset, download error, or
	// a missing/mismatched sidecar — warn and fall back to the pinned Debian +
	// cloud-init path (itself SHA-512 fail-closed). A user-forced image
	// (--image-url / --debian-image) or a disk-manifest pin is honored verbatim
	// and never triggers this fallback.
	if cfg.UseHostedGuestImage {
		return ensureHostedCachedOrDebian(ctx, cfg, path)
	}

	logging.L().Info("downloading base image", "url", cfg.BaseImageURL, "destination", path)
	if err := downloadFile(ctx, cfg.BaseImageURL, path); err != nil {
		return "", err
	}

	if err := verifyImageChecksum(ctx, cfg.BaseImageURL, cfg.BaseImageSHA512, false, path); err != nil {
		// Remove the corrupt download so subsequent runs don't reuse it.
		_ = os.Remove(path)
		return "", err
	}

	if err := ensureRawDiskImage(ctx, path); err != nil {
		return "", err
	}

	logging.L().Info("downloaded base image", "path", path)
	return path, nil
}

// useDebianImage repoints cfg at the Debian genericcloud + cloud-init fallback.
// It is a package var (not a direct config call) solely so tests can redirect the
// fallback to a local httptest server and exercise the hosted->Debian fallback
// hermetically; production wiring is config.UseDebianImage.
var useDebianImage = config.UseDebianImage

// ensureHostedOrDebian downloads and STRICTLY (fail-closed) verifies the
// pre-baked hosted guest image — the default — and on ANY failure (a 404 /
// missing release asset for the arch, a download error, or a missing/unreachable/
// mismatched .sha256 sidecar) emits a clear WARNING and auto-falls-back to the
// pinned Debian genericcloud + first-boot cloud-init path (itself SHA-512
// fail-closed). This preserves the invariant that bladerunner NEVER boots an
// unverified image — it boots verified-hosted or verified-Debian — while never
// bricking a first run on a flaky network or an arch whose hosted asset hasn't
// shipped. It mutates cfg (via useDebianImage) when it falls back so the rest of
// start (status reporting via UseHostedGuestImage) reflects the actual boot. The
// chosen path is logged.
// ensureHostedCachedOrDebian resolves the pre-baked hosted image THROUGH THE
// SHARED CACHE, so a second instance reuses the ~1 GB an earlier one already
// downloaded and converted rather than fetching its own copy.
//
// The cache has existed all along and only disk manifests reached it: the
// default path wrote base-image.raw into each instance's own directory, so on a
// machine that had downloaded the same bytes twice the shared cache sat empty.
// One binary that sometimes shares and sometimes duplicates, depending on how
// the VM was started, is harder to reason about than either behavior alone.
//
// TRUST IS UNCHANGED. The sidecar is fetched first only to learn the cache KEY.
// A miss downloads and verifies against that same sidecar exactly as before; a
// hit reuses bytes whose verification the .ok stamp records. Nothing is trusted
// that was not trusted before, only fetched in a different order — and the cache
// path is in fact stricter, verifying BEFORE conversion rather than after.
//
// An unreachable sidecar falls through to the per-instance path, which makes the
// same Debian fallback it always did: a hosted image that cannot be verified is
// not usable by either route.
func ensureHostedCachedOrDebian(ctx context.Context, cfg *config.Config, path string) (string, error) {
	sha, err := fetchSidecarSHA256(ctx, cfg.BaseImageURL)
	if err != nil {
		// OFFLINE, or the sidecar is briefly unreachable. The bytes may still be
		// here from an earlier verified download, and refusing to use them
		// because the network is down would make this a cache that only works
		// when it is not needed. The pointer records which digest this URL
		// resolved to last time, and the .ok stamp records that those bytes were
		// verified when they were stored — so nothing unverified is reachable
		// through this path.
		if cached, ok := cachedImageForURL(cfg.BaseImageURL); ok {
			logging.L().Warn("hosted image sidecar unreachable; using the previously verified cached image",
				"url", cfg.BaseImageURL+".sha256", "reason", err, "path", cached)
			return cached, nil
		}
		logging.L().Warn("hosted image sidecar unreachable and nothing cached for it",
			"url", cfg.BaseImageURL+".sha256", "reason", err)
		return ensureHostedOrDebian(ctx, cfg, path)
	}

	cached, cacheErr := ensureCachedBaseImage(ctx, cfg, sha)
	if cacheErr == nil {
		rememberImageForURL(cfg.BaseImageURL, sha)
		return cached, nil
	}

	// A cache failure must not cost the user their VM: an unwritable cache
	// directory is not a reason to refuse to boot.
	logging.L().Warn("shared image cache unusable; falling back to a per-instance copy", "reason", cacheErr)
	return ensureHostedOrDebian(ctx, cfg, path)
}

// imagePointerPath is where the digest an image URL last resolved to is
// recorded, keyed by a hash of the URL so it is a safe filename.
func imagePointerPath(imageURL string) string {
	sum := sha256.Sum256([]byte(imageURL))
	return filepath.Join(config.ImageCacheDir(), "by-url", hex.EncodeToString(sum[:])+".digest")
}

// rememberImageForURL records which cached entry this URL resolved to, so a
// later run with no network can find the same bytes. Best effort: failing to
// write a hint must never fail a boot that already succeeded.
func rememberImageForURL(imageURL, sha256hex string) {
	p := imagePointerPath(imageURL)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(p, []byte(sha256hex), 0o644)
}

// cachedImageForURL resolves an image URL to a cached entry without the
// network, reporting false unless the entry AND its verification stamp are both
// present. A pointer to bytes that are gone, or to bytes whose verification did
// not complete, is not a hit.
func cachedImageForURL(imageURL string) (string, bool) {
	raw, err := os.ReadFile(imagePointerPath(imageURL))
	if err != nil {
		return "", false
	}
	digest := strings.TrimSpace(string(raw))
	if digest == "" {
		return "", false
	}
	cachePath := config.ImageCachePath(digest)
	if !util.FileExists(cachePath) || !util.FileExists(cachePath+cacheStampSuffix) {
		return "", false
	}
	return cachePath, true
}

func ensureHostedOrDebian(ctx context.Context, cfg *config.Config, path string) (string, error) {
	logging.L().Info("downloading pre-baked guest image (default)", "url", cfg.BaseImageURL, "destination", path)
	err := downloadFile(ctx, cfg.BaseImageURL, path)
	if err == nil {
		// BaseImageSHA512 is empty for the hosted image; strictSidecar=true makes a
		// missing/unreachable/mismatched .sha256 fatal (fail-closed).
		err = verifyImageChecksum(ctx, cfg.BaseImageURL, "", true, path)
	}
	if err == nil {
		if convErr := ensureRawDiskImage(ctx, path); convErr != nil {
			return "", convErr
		}
		logging.L().Info("using pre-baked guest image (verified)", "path", path)
		return path, nil
	}

	// Discard any partial/corrupt/unverified hosted download before falling back.
	_ = os.Remove(path)

	hostedURL := cfg.BaseImageURL
	if derr := useDebianImage(cfg); derr != nil {
		// No Debian image for this arch either — surface the ORIGINAL hosted error.
		return "", fmt.Errorf("pre-baked guest image unavailable and no Debian fallback for arch %q: %w", cfg.Arch, err)
	}
	logging.L().Warn("pre-baked guest image unavailable; falling back to Debian + cloud-init",
		"reason", err, "hosted_url", hostedURL, "fallback_url", cfg.BaseImageURL)

	logging.L().Info("downloading base image", "url", cfg.BaseImageURL, "destination", path)
	if err := downloadFile(ctx, cfg.BaseImageURL, path); err != nil {
		return "", err
	}
	// The Debian fallback carries the pinned SHA-512, checked fail-closed here.
	if err := verifyImageChecksum(ctx, cfg.BaseImageURL, cfg.BaseImageSHA512, false, path); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	if err := ensureRawDiskImage(ctx, path); err != nil {
		return "", err
	}
	logging.L().Info("using Debian base image (cloud-init path, fallback)", "path", path)
	return path, nil
}

// Cache entry siblings. A content-addressed slot owns three of them, all
// derived from the same path so one digest is one slot.
const (
	// cacheStampSuffix marks an entry whose download was verified and whose
	// conversion completed. Only a stamped entry is reusable.
	cacheStampSuffix = ".ok"
	// cacheStagingSuffix is where the download lands before it is verified,
	// converted, and renamed into the slot.
	cacheStagingSuffix = ".dl"
	// cacheLockSuffix names the flock(2) file that serializes staging into
	// one slot.
	cacheLockSuffix = ".lock"
)

// imageCacheLockRetryInterval is how often a caller waiting for a cache entry
// retries. flock(2) has no context-aware blocking form, so the wait polls the
// non-blocking one and honors cancellation between attempts.
const imageCacheLockRetryInterval = 25 * time.Millisecond

// errImageCacheBusy reports that another bladerunner process holds the shared
// cache entry and this one stopped waiting for it. It exists so that a failure
// caused by LOCAL CONCURRENCY is never worded like a digest mismatch: a
// mismatch says the bytes on the wire were not the bytes that were pinned,
// which sends the user hunting a supply-chain compromise. This says the
// machine is busy with the same download.
var errImageCacheBusy = errors.New("another bladerunner process is staging this base image into the shared cache")

// cacheLock is a held claim on one content-addressed cache slot.
type cacheLock struct {
	file *os.File
}

// lockImageCacheEntry claims exclusive use of one cache slot, waiting for
// whoever holds it until ctx is done.
//
// The cache is GLOBAL (config.ImageCacheDir) while every other lock in the
// system is per-state-directory, so two instances booting cold at the same
// moment share no lock at all and stage through the same fixed paths. The
// claim is keyed on the slot rather than on the whole cache directory, so two
// different images still download in parallel.
//
// flock(2) is the mechanism, as in internal/cartridge: the kernel drops it
// when the holder dies, however it died, so a crashed download leaves a stale
// lock FILE (harmless, reused in place) and never a stale LOCK. Waiting rather
// than racing also means the second caller reuses the first caller's bytes —
// on a metered connection, the difference is a gigabyte.
func lockImageCacheEntry(ctx context.Context, cachePath string) (*cacheLock, error) {
	path := cachePath + cacheLockSuffix
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open image cache lock: %w", err)
	}

	ticker := time.NewTicker(imageCacheLockRetryInterval)
	defer ticker.Stop()
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &cacheLock{file: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("lock image cache entry: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("%w: %w", errImageCacheBusy, ctx.Err())
		case <-ticker.C:
		}
	}
}

// release drops the claim. Closing the descriptor is what releases the kernel
// lock. The lock FILE stays: unlinking it would let a second process create
// and lock a different inode for the same path while this one still believes
// it holds the claim.
func (l *cacheLock) release() {
	if l == nil || l.file == nil {
		return
	}
	_ = l.file.Close()
	l.file = nil
}

// ensureCachedBaseImage materializes the base image into the shared,
// content-addressed cache (<stateDir>/cache/images/<sha256>.raw) and returns its
// path, so the same image is downloaded and converted once and reused instantly
// by every disk slot. The manifest's pinned SHA-256 is verified against the
// downloaded artifact BEFORE the in-place qcow2->raw conversion (the published
// digest is of the qcow2, not the raw). A sibling ".ok" stamp, written only
// after a verified download+convert, marks a trustworthy entry — the converted
// raw's own digest necessarily differs from the qcow2 digest, so it cannot be
// re-verified on reuse, and re-hashing a multi-GB raw on every boot would be
// wasteful.
//
// Staging holds an exclusive claim on the slot, because the cache is shared
// between processes while the staging paths are derived from the digest alone.
// A second caller waits and then takes the warm-cache branch, so the bytes are
// fetched once however many instances boot cold together.
func ensureCachedBaseImage(ctx context.Context, cfg *config.Config, expectedSHA256 string) (string, error) {
	cachePath := config.ImageCachePath(expectedSHA256)
	okStamp := cachePath + cacheStampSuffix
	if util.FileExists(cachePath) && util.FileExists(okStamp) {
		logging.L().Info("using cached base image (content-addressed)", "path", cachePath, "sha256", expectedSHA256)
		return cachePath, nil
	}

	if cfg.BaseImageURL == "" {
		return "", fmt.Errorf("base image url is empty")
	}
	if err := os.MkdirAll(config.ImageCacheDir(), 0o755); err != nil {
		return "", fmt.Errorf("create image cache dir: %w", err)
	}

	lock, err := lockImageCacheEntry(ctx, cachePath)
	if err != nil {
		return "", err
	}
	defer lock.release()

	// Whoever held the claim may have just finished this exact entry, so the
	// warm test is repeated under it. That is also what stops the removal below
	// from deleting an entry another process has already returned to its caller.
	if util.FileExists(cachePath) && util.FileExists(okStamp) {
		logging.L().Info("using cached base image (content-addressed)", "path", cachePath, "sha256", expectedSHA256)
		return cachePath, nil
	}

	// A prior interrupted attempt may have left an unverified entry; clear it.
	_ = os.Remove(cachePath)
	_ = os.Remove(okStamp)

	dlPath := cachePath + cacheStagingSuffix
	logging.L().Info("downloading base image", "url", cfg.BaseImageURL, "destination", cachePath, "sha256", expectedSHA256)
	if err := downloadFile(ctx, cfg.BaseImageURL, dlPath); err != nil {
		_ = os.Remove(dlPath)
		return "", err
	}

	// Verify the downloaded artifact against the manifest's pinned digest BEFORE
	// converting, so the comparison matches the published qcow2 SHA-256.
	got, err := util.FileSHA256(dlPath)
	if err != nil {
		_ = os.Remove(dlPath)
		return "", err
	}
	if !strings.EqualFold(got, expectedSHA256) {
		_ = os.Remove(dlPath)
		return "", fmt.Errorf("base image SHA-256 mismatch: got %s, want %s", got, expectedSHA256)
	}
	logging.L().Info("base image SHA-256 verified", "sha256", got)

	if err := ensureRawDiskImage(ctx, dlPath); err != nil {
		_ = os.Remove(dlPath)
		return "", err
	}
	if err := os.Rename(dlPath, cachePath); err != nil {
		_ = os.Remove(dlPath)
		return "", fmt.Errorf("finalize cached base image: %w", err)
	}
	if err := os.WriteFile(okStamp, nil, 0o644); err != nil {
		return "", fmt.Errorf("write cache stamp: %w", err)
	}

	logging.L().Info("cached base image", "path", cachePath)
	return cachePath, nil
}

func ensureRawDiskImage(ctx context.Context, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open disk image: %w", err)
	}

	header := make([]byte, 4)
	if _, err := io.ReadFull(f, header); err == nil {
		if string(header) == "QFI\xfb" {
			_ = f.Close()
			logging.L().Info("qcow2 image detected, converting to raw format", "path", path)
			if err := convertQcow2ToRaw(ctx, path); err != nil {
				return fmt.Errorf("convert qcow2 to raw: %w", err)
			}
			logging.L().Info("conversion complete", "path", path)
			return nil
		}
	}
	_ = f.Close()
	return nil
}

// rawConvertSuffix names the staging file qemu-img writes the raw image to
// before it is renamed over its source.
const rawConvertSuffix = ".raw"

// convertTimeout bounds one qcow2->raw conversion. A ~1 GB image converts in
// seconds to a few minutes on any supported host, so this length of silence
// means qemu-img is wedged — and a wedged conversion must not hold a boot open
// forever with no way to interrupt it.
const convertTimeout = 30 * time.Minute

// convertQcow2ToRaw converts the qcow2 at qcow2Path into a raw image AT THE
// SAME PATH.
//
// Two properties are load-bearing, because this runs on a CALLER-OWNED path —
// ensureBaseImage passes the user's --base-image-path straight in:
//
//   - The converted output is renamed OVER the source. os.Rename replaces an
//     existing destination, so at every instant qcow2Path holds either the
//     original bytes or the converted ones. Unlinking the source first (as this
//     did) opens a window in which a crash or a failed rename leaves the user
//     with no file at all — a file bladerunner does not own and cannot re-fetch.
//   - The partial output is removed on every error return. Raw expansion of a
//     ~1 GB image is several GB, a full disk is the likeliest failure, and a
//     leaked partial makes the retry more likely to fail the same way.
func convertQcow2ToRaw(ctx context.Context, qcow2Path string) error {
	start := time.Now()

	// Check if qemu-img is available
	if err := RequireQemuImg(); err != nil {
		return err
	}

	rawPath := qcow2Path + rawConvertSuffix
	logging.L().Info("converting disk image", "from", qcow2Path, "to", rawPath)

	converted := false
	defer func() {
		if !converted {
			_ = os.Remove(rawPath)
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, convertTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "qemu-img", "convert", "-f", "qcow2", "-O", "raw", qcow2Path, rawPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img convert failed: %w: %s", err, string(output))
	}

	// Replace the original with the converted image. The rename is the only
	// step: it is atomic, and it never leaves qcow2Path absent.
	if err := os.Rename(rawPath, qcow2Path); err != nil {
		return fmt.Errorf("rename converted image: %w", err)
	}
	converted = true

	logging.L().Info("qcow2 to raw conversion complete", "path", qcow2Path, "elapsed", time.Since(start).Round(time.Millisecond).String())
	return nil
}

func downloadFile(ctx context.Context, url, path string) error {
	start := time.Now()

	// The image is roughly a gigabyte, so the bound is on silence rather than
	// on total duration: the streaming client carries no flat deadline and the
	// watchdog abandons the transfer if the peer stops sending.
	resp, err := httpfetch.Get(ctx, httpfetch.StreamingClient(), url, downloadStallTimeout)
	if err != nil {
		return fmt.Errorf("download base image: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download base image failed: %s", resp.Status)
	}

	// downloadTempSuffix names the file the body streams into before it is
	// renamed into place, so a reader never sees a partial image at path.
	const downloadTempSuffix = ".tmp"
	tmpPath := path + downloadTempSuffix
	_ = os.Remove(tmpPath)

	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create temp image file: %w", err)
	}

	// A failed download must not strand its partial body. The removal is
	// disarmed once the bytes are renamed into place.
	moved := false
	defer func() {
		if !moved {
			_ = os.Remove(tmpPath)
		}
	}()

	progress := logging.NewByteProgress("Downloading base image", resp.ContentLength)
	if _, err := io.Copy(f, io.TeeReader(resp.Body, progress)); err != nil {
		progress.Fail(err)
		_ = f.Close()
		return fmt.Errorf("write image to disk: %w", err)
	}
	progress.Finish()
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temp image file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("move downloaded image into place: %w", err)
	}
	moved = true
	logging.L().Info("download complete", "url", url, "path", path, "elapsed", time.Since(start).Round(time.Millisecond).String())
	return nil
}
