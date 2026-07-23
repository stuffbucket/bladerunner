package vm

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/logging"
	"github.com/stuffbucket/bladerunner/internal/util"
)

// downloadClient is the HTTP client used for image downloads. It deliberately
// sets NO total request timeout — a legitimate multi-GB image can take many
// minutes over a slow link and must not be killed mid-stream. Instead it bounds
// the failure modes that actually hang forever: a dead TCP connect, a stalled
// TLS handshake, and a server that accepts the connection but never sends
// response headers. Once bytes are flowing, io.Copy progress is the liveness
// signal; a truly stalled body is bounded by IdleConnTimeout / the OS TCP
// keepalive. Redirect following is left at the default (up to 10 hops).
const (
	downloadDialTimeout    = 30 * time.Second
	downloadKeepAlive      = 30 * time.Second
	downloadTLSTimeout     = 30 * time.Second
	downloadRespHdrTimeout = 60 * time.Second
	downloadExpectContinue = 5 * time.Second
	downloadIdleTimeout    = 90 * time.Second
)

var downloadClient = &http.Client{
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   downloadDialTimeout,
			KeepAlive: downloadKeepAlive,
		}).DialContext,
		TLSHandshakeTimeout:   downloadTLSTimeout,
		ResponseHeaderTimeout: downloadRespHdrTimeout,
		ExpectContinueTimeout: downloadExpectContinue,
		IdleConnTimeout:       downloadIdleTimeout,
		ForceAttemptHTTP2:     true,
	},
}

// downloadMaxAttempts bounds the exponential-backoff retry loop in downloadFile.
const downloadMaxAttempts = 4

// sidecarMaxAttempts bounds the short retry loop in fetchSidecarSHA256. A
// captive portal / silent TCP drop must not hang boot forever (the bounded
// downloadClient caps the connect/TLS/header stall), and a transient blip
// should retry a couple of times — but a definitive 404 fails fast (it means
// "no sidecar published", which the caller must decide on), so it is never
// retried.
const sidecarMaxAttempts = 3

// sidecarInitialBackoff is the first backoff between failed sidecar fetches; it
// doubles each retry, mirroring downloadFile's exponential-backoff pattern.
const sidecarInitialBackoff = 500 * time.Millisecond

// requireVerifiedCustomImage, when true, makes a user-supplied --image-url fail
// CLOSED: absent an explicit --image-sha256 (threaded via
// BaseImageExpectedSHA256), the download must be covered by a present+matching
// .sha256 sidecar, and a missing/unreachable/mismatched sidecar refuses to boot
// rather than warning-and-continuing. It is armed by the start command
// (SetRequireVerifiedCustomImage) only for an explicit --image-url, so the
// Debian/hosted defaults and disk-manifest pins keep their own verification
// semantics.
var requireVerifiedCustomImage bool

// SetRequireVerifiedCustomImage arms (or disarms) fail-closed verification for a
// user-supplied --image-url. The start command calls it once, before
// EnsureBaseImage, when the user passed --image-url without an --image-sha256 so
// the run refuses to boot an unverified custom image.
func SetRequireVerifiedCustomImage(v bool) { requireVerifiedCustomImage = v }

// bytesPerGiB is 1 GiB in bytes, used by the disk-space preflight.
const bytesPerGiB = int64(1) << 30

// checkDiskSpace fails closed with an actionable message when avail is less than
// the need required to materialize the VM disk. It is a pure helper (no I/O) so
// the arithmetic is unit-testable; path is used only for the message. (The
// darwin free-space probe lives in assets_darwin.go; this is portable.)
func checkDiskSpace(need, avail int64, path string) error {
	if avail < need {
		needGiB := float64(need) / float64(bytesPerGiB)
		haveGiB := float64(avail) / float64(bytesPerGiB)
		return fmt.Errorf(
			"not enough disk space to create the VM disk: need ~%.1f GiB, have %.1f GiB free at %s — free space or lower --disk",
			needGiB, haveGiB, path)
	}
	return nil
}

// transientDownloadError reports whether err from an HTTP download attempt is
// worth retrying. Connection refused/reset, timeouts, and unexpected EOFs are
// transient network blips; a fatalHTTPError (a definitive 4xx like 404/403/401)
// is not — it should surface immediately so the caller can fall back or fail
// fast rather than burning ~seconds of backoff on a URL that will never work.
func transientDownloadError(err error) bool {
	if err == nil {
		return false
	}
	var fatal *fatalHTTPError
	if errors.As(err, &fatal) {
		return false
	}
	var terminal *terminalError
	if errors.As(err, &terminal) {
		return false
	}
	// Context cancellation/deadline from the caller is not a retryable blip.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	// net.Error (dial/TLS/response-header timeouts) and the syscall-level
	// connection refused/reset errors all warrant a retry.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return true
}

// fatalHTTPError marks an HTTP response whose status is definitively terminal
// (a 4xx other than 429): retrying will not change the outcome, so the download
// loop returns it immediately for the caller to handle (fall back / fail fast).
type fatalHTTPError struct {
	status string
	code   int
}

func (e *fatalHTTPError) Error() string {
	return fmt.Sprintf("download base image failed: %s", e.status)
}

// terminalError wraps an error that must NOT be retried even though it is not an
// HTTP status (e.g. a malformed sidecar body: retrying the same URL yields the
// same bad bytes). transientDownloadError classifies it non-transient so the
// bounded retry loops fail fast instead of burning backoff.
type terminalError struct{ err error }

func (e *terminalError) Error() string { return e.err.Error() }
func (e *terminalError) Unwrap() error { return e.err }

// fetchSidecarSHA256 fetches a "<url>.sha256" sidecar and returns the
// lowercased hex digest. The sidecar may be either bare hex or the
// `sha256sum` format ("<hex>  <filename>"); only the first whitespace-
// separated token is used. Returns "" with no error if the sidecar
// 404s (caller decides whether that's acceptable).
//
// It uses the package's bounded downloadClient (dial/TLS/idle timeouts) rather
// than http.DefaultClient so a captive portal / silent TCP drop cannot hang
// boot forever, and wraps the fetch in a short exponential-backoff retry so a
// transient network blip retries — but a definitive 404 (via fatalHTTPError)
// fails fast rather than burning backoff on a sidecar that will never appear.
func fetchSidecarSHA256(ctx context.Context, imageURL string) (string, error) {
	backoff := sidecarInitialBackoff
	var lastErr error
	for attempt := 1; attempt <= sidecarMaxAttempts; attempt++ {
		digest, err := fetchSidecarSHA256Once(ctx, imageURL)
		if err == nil {
			return digest, nil
		}
		lastErr = err
		if !transientDownloadError(err) {
			// A 404 is surfaced by fetchSidecarSHA256Once as ("", nil), so any
			// non-transient error here (a definitive non-2xx, a malformed sidecar)
			// is terminal: retrying will not change the outcome.
			return "", err
		}
		if attempt == sidecarMaxAttempts {
			break
		}
		logging.L().Warn("sidecar checksum fetch failed, retrying",
			"url", imageURL+".sha256", "attempt", attempt, "max_attempts", sidecarMaxAttempts,
			"backoff", backoff.String(), "err", err)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	return "", fmt.Errorf("fetch sidecar checksum after %d attempts: %w", sidecarMaxAttempts, lastErr)
}

// fetchSidecarSHA256Once performs a single sidecar fetch attempt over the
// bounded downloadClient. A 404 returns ("", nil); a definitive non-2xx returns
// a fatalHTTPError (so the retry loop fails fast); a transient network error is
// returned as-is for the loop to retry.
func fetchSidecarSHA256Once(ctx context.Context, imageURL string) (string, error) {
	sidecarURL := imageURL + ".sha256"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sidecarURL, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("create sidecar request: %w", err)
	}
	resp, err := downloadClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch sidecar checksum: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// A definitive non-2xx (other than 429) is terminal; mark it fatal so the
		// retry loop does not burn backoff on it. 5xx / 429 stay transient.
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			return "", &fatalHTTPError{status: resp.Status, code: resp.StatusCode}
		}
		return "", fmt.Errorf("fetch sidecar checksum: %s", resp.Status)
	}

	const maxSidecarBytes = 4096
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxSidecarBytes))
	if err != nil {
		return "", fmt.Errorf("read sidecar checksum: %w", err)
	}
	first := strings.Fields(strings.TrimSpace(string(b)))
	if len(first) == 0 {
		return "", &terminalError{fmt.Errorf("sidecar checksum is empty")}
	}
	digest := strings.ToLower(first[0])
	if len(digest) != sha256.Size*2 {
		return "", &terminalError{fmt.Errorf("sidecar checksum has unexpected length: %d", len(digest))}
	}
	for _, r := range digest {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return "", &terminalError{fmt.Errorf("sidecar checksum is not hex: %q", digest)}
		}
	}
	return digest, nil
}

// fileSHA256 returns the hex-encoded SHA-256 digest of the file at path.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open for sha256: %w", err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash file: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

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
//     published .sha256; or a user-supplied --image-url with fail-closed custom
//     verification armed): FAIL CLOSED. A mismatch, a missing/404 sidecar, or an
//     unreachable sidecar host are all fatal — parity with the pinned-Debian
//     SHA-512 path. The hosted image is release-managed, so its checksum must
//     always be present and correct; a gap means "do not boot", not "boot
//     anyway".
//   - strictSidecar=false (the Debian genericcloud fallback, verified via its
//     embedded SHA-512, or a legacy tolerant custom URL): a missing or unreachable
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
			return fmt.Errorf("image sidecar SHA-256 unreachable (%s): refusing to boot an unverified image — pass --image-sha256 <hex> or publish a %s sidecar: %w",
				imageURL+".sha256", imageURL+".sha256", err)
		}
		logging.L().Warn("sidecar SHA-256 fetch failed, continuing without verification",
			"url", imageURL+".sha256", "err", err)
		return nil
	}
	if want == "" {
		if strictSidecar {
			return fmt.Errorf("image sidecar SHA-256 missing (%s): refusing to boot an unverified custom image — pass --image-sha256 <hex> or publish a %s sidecar",
				imageURL+".sha256", imageURL+".sha256")
		}
		logging.L().Warn("sidecar SHA-256 not present, skipping verification",
			"url", imageURL+".sha256")
		return nil
	}
	got, err := fileSHA256(path)
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
		_ = os.Remove(dst)
		return fmt.Errorf("copy base image into cartridge: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("close root.img: %w", err)
	}

	if err := RequireQemuImg(); err != nil {
		return err
	}
	targetSize := fmt.Sprintf("%dG", diskSizeGiB)
	cmd := exec.Command("qemu-img", "resize", "-f", "raw", dst, targetSize)
	if output, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("qemu-img resize failed: %w: %s", err, string(output))
	}
	return nil
}

// verifyLocalImageSHA256 fail-closed verifies the local file at path against an
// expected SHA-256 hex digest. An empty expected is a no-op (the user controls a
// local --image-path, so the hash is optional); a non-empty expected that does
// not match is fatal so a pinned local image never boots tampered.
func verifyLocalImageSHA256(path, expectedSHA256 string) error {
	if expectedSHA256 == "" {
		return nil
	}
	got, err := fileSHA256(path)
	if err != nil {
		return err
	}
	if !strings.EqualFold(got, expectedSHA256) {
		return fmt.Errorf("base image SHA-256 mismatch: got %s, want %s", got, expectedSHA256)
	}
	logging.L().Info("base image SHA-256 verified (--image-path)", "sha256", got, "path", path)
	return nil
}

func ensureBaseImage(ctx context.Context, cfg *config.Config) (string, error) {
	if cfg.BaseImagePath != "" {
		if !util.FileExists(cfg.BaseImagePath) {
			return "", fmt.Errorf("base image path does not exist: %s", cfg.BaseImagePath)
		}
		// The user controls the local file, so a hash is optional — but when one is
		// supplied (--image-path-sha256, threaded via BaseImageExpectedSHA256),
		// verify it fail-closed BEFORE any qcow2->raw conversion so the digest
		// matches the file the user pinned.
		if err := verifyLocalImageSHA256(cfg.BaseImagePath, cfg.BaseImageExpectedSHA256); err != nil {
			return "", err
		}
		if err := ensureRawDiskImage(cfg.BaseImagePath); err != nil {
			return "", err
		}
		logging.L().Info("using provided base image", "path", cfg.BaseImagePath)
		return cfg.BaseImagePath, nil
	}

	// When a disk manifest pins an explicit SHA-256 of the downloaded artifact,
	// materialize the base image once into the shared content-addressed cache and
	// reuse it across every disk slot.
	if cfg.BaseImageExpectedSHA256 != "" {
		return ensureCachedBaseImage(ctx, cfg)
	}

	path := filepath.Join(cfg.VMDir, "base-image.raw")
	if util.FileExists(path) {
		if err := ensureRawDiskImage(path); err != nil {
			return "", err
		}
		// Gate the cached file: a truncated/corrupt cache entry (from a prior
		// interrupted convert/download) is rebuilt rather than silently booted.
		if err := verifyImageIntegrity(path); err != nil {
			logging.L().Warn("cached base image failed integrity check; re-downloading",
				"path", path, "err", err)
			_ = os.Remove(path)
		} else {
			logging.L().Info("using cached base image", "path", path)
			return path, nil
		}
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
		return ensureHostedOrDebian(ctx, cfg, path)
	}

	logging.L().Info("downloading base image", "url", cfg.BaseImageURL, "destination", path)
	if err := downloadFile(ctx, cfg.BaseImageURL, path); err != nil {
		return "", err
	}

	// For a custom --image-url with no embedded SHA-512 and no pinned SHA-256
	// (which would have routed to ensureCachedBaseImage), verification is
	// sidecar-based. requireVerifiedCustomImage makes that fail-closed: a
	// missing/unreachable/mismatched .sha256 refuses to boot rather than warning
	// and continuing. The Debian fallback carries an embedded SHA-512 and so takes
	// the strictSidecar-independent SHA-512 branch regardless.
	if err := verifyImageChecksum(ctx, cfg.BaseImageURL, cfg.BaseImageSHA512, requireVerifiedCustomImage, path); err != nil {
		// Remove the corrupt download so subsequent runs don't reuse it.
		_ = os.Remove(path)
		return "", err
	}

	if err := ensureRawDiskImage(path); err != nil {
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
func ensureHostedOrDebian(ctx context.Context, cfg *config.Config, path string) (string, error) {
	logging.L().Info("downloading pre-baked guest image (default)", "url", cfg.BaseImageURL, "destination", path)
	err := downloadFile(ctx, cfg.BaseImageURL, path)
	if err == nil {
		// BaseImageSHA512 is empty for the hosted image; strictSidecar=true makes a
		// missing/unreachable/mismatched .sha256 fatal (fail-closed).
		err = verifyImageChecksum(ctx, cfg.BaseImageURL, "", true, path)
	}
	if err == nil {
		if convErr := ensureRawDiskImage(path); convErr != nil {
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
	if err := ensureRawDiskImage(path); err != nil {
		return "", err
	}
	logging.L().Info("using Debian base image (cloud-init path, fallback)", "path", path)
	return path, nil
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
func ensureCachedBaseImage(ctx context.Context, cfg *config.Config) (string, error) {
	cachePath := config.ImageCachePath(cfg.BaseImageExpectedSHA256)
	okStamp := cachePath + ".ok"
	if util.FileExists(cachePath) && util.FileExists(okStamp) {
		// The .ok stamp marks a verified download+convert, but a subsequent disk
		// event (partial write, truncation) could still corrupt the cached raw;
		// gate it so a bad entry is rebuilt rather than booted.
		if err := verifyImageIntegrity(cachePath); err != nil {
			logging.L().Warn("cached base image failed integrity check; rebuilding",
				"path", cachePath, "sha256", cfg.BaseImageExpectedSHA256, "err", err)
			_ = os.Remove(cachePath)
			_ = os.Remove(okStamp)
		} else {
			logging.L().Info("using cached base image (content-addressed)", "path", cachePath, "sha256", cfg.BaseImageExpectedSHA256)
			return cachePath, nil
		}
	}

	if cfg.BaseImageURL == "" {
		return "", fmt.Errorf("base image url is empty")
	}
	if err := os.MkdirAll(config.ImageCacheDir(), 0o755); err != nil {
		return "", fmt.Errorf("create image cache dir: %w", err)
	}

	// A prior interrupted attempt may have left an unverified entry; clear it.
	_ = os.Remove(cachePath)
	_ = os.Remove(okStamp)

	dlPath := cachePath + ".dl"
	logging.L().Info("downloading base image", "url", cfg.BaseImageURL, "destination", cachePath, "sha256", cfg.BaseImageExpectedSHA256)
	if err := downloadFile(ctx, cfg.BaseImageURL, dlPath); err != nil {
		_ = os.Remove(dlPath)
		return "", err
	}

	// Verify the downloaded artifact against the manifest's pinned digest BEFORE
	// converting, so the comparison matches the published qcow2 SHA-256.
	got, err := fileSHA256(dlPath)
	if err != nil {
		_ = os.Remove(dlPath)
		return "", err
	}
	if !strings.EqualFold(got, cfg.BaseImageExpectedSHA256) {
		_ = os.Remove(dlPath)
		return "", fmt.Errorf("base image SHA-256 mismatch: got %s, want %s", got, cfg.BaseImageExpectedSHA256)
	}
	logging.L().Info("base image SHA-256 verified (pinned by disk)", "sha256", got)

	if err := ensureRawDiskImage(dlPath); err != nil {
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

func ensureRawDiskImage(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open disk image: %w", err)
	}

	header := make([]byte, 4)
	if _, err := io.ReadFull(f, header); err == nil {
		if string(header) == "QFI\xfb" {
			_ = f.Close()
			logging.L().Info("qcow2 image detected, converting to raw format", "path", path)
			if err := convertQcow2ToRaw(path); err != nil {
				return fmt.Errorf("convert qcow2 to raw: %w", err)
			}
			logging.L().Info("conversion complete", "path", path)
			return nil
		}
	}
	_ = f.Close()
	return nil
}

func convertQcow2ToRaw(qcow2Path string) error {
	start := time.Now()

	// Check if qemu-img is available
	if err := RequireQemuImg(); err != nil {
		return err
	}

	rawPath := qcow2Path + ".raw"
	logging.L().Info("converting disk image", "from", qcow2Path, "to", rawPath)

	cmd := exec.Command("qemu-img", "convert", "-f", "qcow2", "-O", "raw", qcow2Path, rawPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		// A partial/truncated .raw would otherwise linger and get renamed over a
		// good source on a later run; remove it so the conversion is retried clean.
		_ = os.Remove(rawPath)
		return fmt.Errorf("qemu-img convert failed: %w: %s", err, string(output))
	}

	// Replace original with converted image
	if err := os.Remove(qcow2Path); err != nil {
		logging.L().Warn("failed to remove qcow2 file", "path", qcow2Path, "err", err)
	}
	if err := os.Rename(rawPath, qcow2Path); err != nil {
		return fmt.Errorf("rename converted image: %w", err)
	}

	logging.L().Info("qcow2 to raw conversion complete", "path", qcow2Path, "elapsed", time.Since(start).Round(time.Millisecond).String())
	return nil
}

// verifyImageIntegrity is a fast, best-effort gate that a materialized image or
// disk is not obviously truncated/corrupt, so a bad cached file is rebuilt
// rather than silently booted. It never does a full multi-GB content scan:
//
//   - It always runs a cheap size>0 + first-byte readability probe.
//   - When qemu-img is available it additionally runs `qemu-img check -q`, but
//     only for formats that actually support a structural check (qcow2, qed,
//     …). RAW images — which is what our convert/copy pipeline always produces —
//     have no internal structure to walk; `qemu-img check` on a raw legitimately
//     reports "does not support checks", so for raw the probe above IS the gate.
//     A qemu-img that can't even open/identify the file (info fails) is treated
//     as corrupt.
//   - When qemu-img is absent the size/readability probe alone is the gate.
//
// A nil error means "usable"; a non-nil error means "rebuild me".
func verifyImageIntegrity(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat image %s: %w", path, err)
	}
	if fi.Size() == 0 {
		return fmt.Errorf("image %s is empty (zero bytes)", path)
	}

	// Cheap readability probe (also the whole gate when qemu-img is unavailable).
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open image %s: %w", path, err)
	}
	if _, err := f.Read(make([]byte, 1)); err != nil {
		_ = f.Close()
		return fmt.Errorf("read image %s: %w", path, err)
	}
	_ = f.Close()

	haveQemuImg := RequireQemuImg() == nil
	if !haveQemuImg {
		return nil
	}

	// qemu-img must at least be able to open and identify the file; a failure here
	// means the file is unreadable/unrecognizable (truncated header, etc.).
	format, err := qemuImgFormat(path)
	if err != nil {
		return fmt.Errorf("qemu-img could not identify image %s: %w", path, err)
	}
	// `qemu-img check` only applies to formats with checkable structure. RAW has
	// none, so the probe above is sufficient; running check on raw returns a
	// spurious "does not support checks" error.
	if format == "raw" || format == "" {
		return nil
	}
	cmd := exec.Command("qemu-img", "check", "-q", path)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img check reported a corrupt image %s: %w: %s", path, err, strings.TrimSpace(string(output)))
	}
	return nil
}

// qemuImgFormat returns the file format qemu-img detects for path (e.g. "raw",
// "qcow2"). An error means qemu-img could not open/identify the file at all.
func qemuImgFormat(path string) (string, error) {
	out, err := exec.Command("qemu-img", "info", path).Output()
	if err != nil {
		return "", err
	}
	// The plain (non-JSON) output has exactly one top-level "file format:" line,
	// which avoids the nested-child ambiguity of --output=json (whose first
	// "format" is the protocol node's "file").
	for line := range strings.SplitSeq(string(out), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "file format:"); ok {
			return strings.TrimSpace(rest), nil
		}
	}
	return "", nil
}

// downloadFile downloads url to path, retrying transient failures (connection
// refused/reset, dial/TLS/response-header timeouts, 5xx, 429, unexpected EOF)
// with exponential backoff. Definitively fatal responses (a 4xx like 404/403/
// 401) return immediately so the caller can fall back or fail fast instead of
// burning backoff on a URL that will never work. On any failure the partial
// ".tmp" is removed so a corrupt artifact is never left behind for reuse.
func downloadFile(ctx context.Context, url, path string) error {
	start := time.Now()
	backoff := time.Second
	var lastErr error
	for attempt := 1; attempt <= downloadMaxAttempts; attempt++ {
		err := downloadOnce(ctx, url, path)
		if err == nil {
			logging.L().Info("download complete", "url", url, "path", path,
				"attempts", attempt, "elapsed", time.Since(start).Round(time.Millisecond).String())
			return nil
		}
		lastErr = err
		if !transientDownloadError(err) {
			return err
		}
		if attempt == downloadMaxAttempts {
			break
		}
		logging.L().Warn("base image download failed, retrying",
			"url", url, "attempt", attempt, "max_attempts", downloadMaxAttempts,
			"backoff", backoff.String(), "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	return fmt.Errorf("download base image failed after %d attempts: %w", downloadMaxAttempts, lastErr)
}

// downloadOnce performs a single download attempt into path+".tmp", renaming it
// into place only on success. Any error removes the partial ".tmp" so a
// truncated artifact is never reused by a later boot.
func downloadOnce(ctx context.Context, url, path string) error {
	tmpPath := path + ".tmp"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return fmt.Errorf("create download request: %w", err)
	}

	resp, err := downloadClient.Do(req)
	if err != nil {
		return fmt.Errorf("download base image: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 4xx (except 429 Too Many Requests) is terminal: retrying won't help.
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			return &fatalHTTPError{status: resp.Status, code: resp.StatusCode}
		}
		return fmt.Errorf("download base image failed: %s", resp.Status)
	}

	_ = os.Remove(tmpPath)
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create temp image file: %w", err)
	}

	progress := logging.NewByteProgress("Downloading base image", resp.ContentLength)
	if _, err := io.Copy(f, io.TeeReader(resp.Body, progress)); err != nil {
		progress.Fail(err)
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write image to disk: %w", err)
	}
	progress.Finish()
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp image file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("move downloaded image into place: %w", err)
	}
	return nil
}
