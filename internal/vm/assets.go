package vm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/logging"
	"github.com/stuffbucket/bladerunner/internal/util"
)

// isGitHubReleaseURL reports whether url points at a github.com release
// download. Used to relax sidecar-checksum strictness during the period
// before the first guest-image release ships a .sha256 sidecar.
func isGitHubReleaseURL(url string) bool {
	return strings.Contains(url, "github.com/") && strings.Contains(url, "/releases/")
}

// fetchSidecarSHA256 fetches a "<url>.sha256" sidecar and returns the
// lowercased hex digest. The sidecar may be either bare hex or the
// `sha256sum` format ("<hex>  <filename>"); only the first whitespace-
// separated token is used. Returns "" with no error if the sidecar
// 404s (caller decides whether that's acceptable).
func fetchSidecarSHA256(ctx context.Context, imageURL string) (string, error) {
	sidecarURL := imageURL + ".sha256"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sidecarURL, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("create sidecar request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
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

// verifyImageChecksum compares the downloaded image at path against the
// sidecar checksum hosted at imageURL+".sha256". A missing or unreachable
// sidecar is logged at WARN and skipped — many upstream image hosts
// (Debian's cloud.debian.org, GitHub Releases pre-first-publish) don't
// publish per-image .sha256 sidecars, and blocking boot on their absence
// regresses the default user experience. Mismatched sidecars remain fatal.
// See issue: SHA256SUMS-style combined manifests are not yet parsed.
func verifyImageChecksum(ctx context.Context, imageURL, path string) error {
	want, err := fetchSidecarSHA256(ctx, imageURL)
	if err != nil {
		logging.L().Warn("sidecar SHA-256 fetch failed, continuing without verification",
			"url", imageURL+".sha256", "err", err)
		return nil
	}
	if want == "" {
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

func ensureVMDir(cfg *config.Config) error {
	start := time.Now()
	if err := os.MkdirAll(cfg.VMDir, 0o755); err != nil {
		return fmt.Errorf("create vm directory %s: %w", cfg.VMDir, err)
	}
	logging.L().Info("ensured VM directory", "path", cfg.VMDir, "elapsed", time.Since(start).Round(time.Millisecond).String())
	return nil
}

func ensureBaseImage(ctx context.Context, cfg *config.Config) (string, error) {
	if cfg.BaseImagePath != "" {
		if !util.FileExists(cfg.BaseImagePath) {
			return "", fmt.Errorf("base image path does not exist: %s", cfg.BaseImagePath)
		}
		if err := ensureRawDiskImage(cfg.BaseImagePath); err != nil {
			return "", err
		}
		logging.L().Info("using provided base image", "path", cfg.BaseImagePath)
		return cfg.BaseImagePath, nil
	}

	path := filepath.Join(cfg.VMDir, "base-image.raw")
	if util.FileExists(path) {
		if err := ensureRawDiskImage(path); err != nil {
			return "", err
		}
		logging.L().Info("using cached base image", "path", path)
		return path, nil
	}

	if cfg.BaseImageURL == "" {
		return "", fmt.Errorf("base image url is empty")
	}

	logging.L().Info("downloading base image", "url", cfg.BaseImageURL, "destination", path)
	if err := downloadFile(ctx, cfg.BaseImageURL, path); err != nil {
		return "", err
	}

	if err := verifyImageChecksum(ctx, cfg.BaseImageURL, path); err != nil {
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

func ensureMainDisk(cfg *config.Config, baseImagePath string) error {
	if util.FileExists(cfg.DiskPath) {
		logging.L().Info("reusing existing VM disk", "path", cfg.DiskPath)
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(cfg.DiskPath), 0o755); err != nil {
		return fmt.Errorf("create disk parent: %w", err)
	}

	// Copy base image to disk location.
	in, err := os.Open(baseImagePath)
	if err != nil {
		return fmt.Errorf("open base image: %w", err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(cfg.DiskPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create disk image: %w", err)
	}

	sourceInfo, _ := in.Stat()
	sourceSize := int64(0)
	if sourceInfo != nil {
		sourceSize = sourceInfo.Size()
	}
	progress := logging.NewByteProgress("Creating main disk", sourceSize)
	_, err = io.Copy(out, io.TeeReader(in, progress))
	if err != nil {
		progress.Fail(err)
		return fmt.Errorf("copy base image to disk: %w", err)
	}
	progress.Finish()
	if err := out.Close(); err != nil {
		return fmt.Errorf("close disk image: %w", err)
	}

	// Use qemu-img to resize the disk. This correctly updates the GPT backup
	// header and avoids corrupting the partition table (unlike raw truncate).
	targetSize := fmt.Sprintf("%dG", cfg.DiskSizeGiB)
	logging.L().Info("resizing disk image", "path", cfg.DiskPath, "target", targetSize)
	cmd := exec.Command("qemu-img", "resize", "-f", "raw", cfg.DiskPath, targetSize)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img resize failed: %w: %s", err, string(output))
	}

	logging.L().Info("created VM disk image", "path", cfg.DiskPath, "size", targetSize)
	return nil
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
	if _, err := exec.LookPath("qemu-img"); err != nil {
		return fmt.Errorf("qemu-img not found in PATH (install with: brew install qemu): %w", err)
	}

	rawPath := qcow2Path + ".raw"
	logging.L().Info("converting disk image", "from", qcow2Path, "to", rawPath)

	cmd := exec.Command("qemu-img", "convert", "-f", "qcow2", "-O", "raw", qcow2Path, rawPath)
	if output, err := cmd.CombinedOutput(); err != nil {
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

// Image-download tuning. cloud.debian.org 302-redirects to regional mirrors,
// some of which accept the TLS connection but never send a response (or stall
// mid-body). Without bounds the download hangs forever; these timeouts make it
// fail fast so the retry loop can land on a healthy mirror.
const (
	imageConnectTimeout  = 30 * time.Second // dial + TLS handshake
	imageHeaderTimeout   = 45 * time.Second // server must start responding
	imageStallTimeout    = 60 * time.Second // max gap between received chunks
	imageKeepAlive       = 30 * time.Second
	imageIdleConnTimeout = 90 * time.Second
	imageRetryBackoff    = 2 * time.Second
	imageDownloadRetries = 3
)

func imageHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   imageConnectTimeout,
				KeepAlive: imageKeepAlive,
			}).DialContext,
			TLSHandshakeTimeout:   imageConnectTimeout,
			ResponseHeaderTimeout: imageHeaderTimeout,
			IdleConnTimeout:       imageIdleConnTimeout,
			ForceAttemptHTTP2:     true,
		},
	}
}

// downloadFile fetches url to path, retrying on transient failures (each retry
// re-resolves the cloud.debian.org redirect and often lands on a different
// mirror). Connection, header, and stall timeouts bound every attempt.
func downloadFile(ctx context.Context, url, path string) error {
	var lastErr error
	for attempt := 1; attempt <= imageDownloadRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := downloadOnce(ctx, url, path)
		if err == nil {
			return nil
		}
		lastErr = err
		logging.L().Warn("base image download attempt failed; retrying",
			"attempt", attempt, "max", imageDownloadRetries, "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt) * imageRetryBackoff):
		}
	}
	return fmt.Errorf("download base image after %d attempts: %w", imageDownloadRetries, lastErr)
}

func downloadOnce(ctx context.Context, url, path string) error {
	start := time.Now()
	// A cancelable child context lets the stall watchdog abort an in-flight read.
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return fmt.Errorf("create download request: %w", err)
	}

	resp, err := imageHTTPClient().Do(req)
	if err != nil {
		return fmt.Errorf("connect/headers: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status: %s", resp.Status)
	}

	tmpPath := path + ".tmp"
	_ = os.Remove(tmpPath)

	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create temp image file: %w", err)
	}

	progress := logging.NewByteProgress("Downloading base image", resp.ContentLength)
	sr := newStallReader(io.TeeReader(resp.Body, progress), cancel, imageStallTimeout)
	sr.arm()
	_, copyErr := io.Copy(f, sr)
	sr.disarm()
	closeErr := f.Close()

	if copyErr != nil {
		progress.Fail(copyErr)
		_ = os.Remove(tmpPath)
		if sr.stalled() {
			return fmt.Errorf("download stalled: no data for %s", imageStallTimeout)
		}
		return fmt.Errorf("write image to disk: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp image file: %w", closeErr)
	}
	progress.Finish()

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("move downloaded image into place: %w", err)
	}
	logging.L().Info("download complete", "url", url, "path", path, "elapsed", time.Since(start).Round(time.Millisecond).String())
	return nil
}

// stallReader wraps a reader and cancels the request if no bytes are read for
// the configured timeout, catching mirrors that hang mid-transfer.
type stallReader struct {
	r       io.Reader
	cancel  context.CancelFunc
	timeout time.Duration
	timer   *time.Timer
	didFire atomic.Bool
}

func newStallReader(r io.Reader, cancel context.CancelFunc, timeout time.Duration) *stallReader {
	return &stallReader{r: r, cancel: cancel, timeout: timeout}
}

func (s *stallReader) arm() {
	s.timer = time.AfterFunc(s.timeout, func() {
		s.didFire.Store(true)
		s.cancel()
	})
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 && s.timer != nil {
		s.timer.Reset(s.timeout)
	}
	return n, err
}

func (s *stallReader) disarm() {
	if s.timer != nil {
		s.timer.Stop()
	}
}

func (s *stallReader) stalled() bool { return s.didFire.Load() }
