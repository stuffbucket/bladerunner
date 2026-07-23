package vm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/util"
)

func writeTempFile(t *testing.T, data []byte) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "image.qcow2")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return path
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// fakeServer serves the image bytes at /image and an optional sidecar at
// /image.sha256. If sidecar is "404", returns 404 for the sidecar.
func fakeServer(t *testing.T, image []byte, sidecar string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/image", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(image)
	})
	mux.HandleFunc("/image.sha256", func(w http.ResponseWriter, _ *http.Request) {
		if sidecar == "404" {
			http.NotFound(w, nil)
			return
		}
		_, _ = w.Write([]byte(sidecar))
	})
	return httptest.NewServer(mux)
}

func TestFetchSidecarSHA256_Valid(t *testing.T) {
	digest := strings.Repeat("a", 64)
	srv := fakeServer(t, nil, digest+"\n")
	defer srv.Close()

	got, err := fetchSidecarSHA256(context.Background(), srv.URL+"/image")
	if err != nil {
		t.Fatalf("fetchSidecarSHA256 error = %v", err)
	}
	if got != digest {
		t.Errorf("got %q, want %q", got, digest)
	}
}

func TestFetchSidecarSHA256_Sha256sumFormat(t *testing.T) {
	digest := strings.Repeat("b", 64)
	srv := fakeServer(t, nil, digest+"  bladerunner-guest-arm64.qcow2\n")
	defer srv.Close()

	got, err := fetchSidecarSHA256(context.Background(), srv.URL+"/image")
	if err != nil {
		t.Fatalf("fetchSidecarSHA256 error = %v", err)
	}
	if got != digest {
		t.Errorf("got %q, want %q", got, digest)
	}
}

func TestFetchSidecarSHA256_404(t *testing.T) {
	srv := fakeServer(t, nil, "404")
	defer srv.Close()

	got, err := fetchSidecarSHA256(context.Background(), srv.URL+"/image")
	if err != nil {
		t.Fatalf("fetchSidecarSHA256 expected nil error for 404, got %v", err)
	}
	if got != "" {
		t.Errorf("expected empty digest on 404, got %q", got)
	}
}

func TestFetchSidecarSHA256_BadHex(t *testing.T) {
	srv := fakeServer(t, nil, "nothex"+strings.Repeat("0", 58))
	defer srv.Close()

	if _, err := fetchSidecarSHA256(context.Background(), srv.URL+"/image"); err == nil {
		t.Error("expected error for non-hex sidecar")
	}
}

func TestFileSHA256(t *testing.T) {
	data := []byte("hello bladerunner")
	path := writeTempFile(t, data)
	got, err := fileSHA256(path)
	if err != nil {
		t.Fatalf("fileSHA256 error = %v", err)
	}
	if got != sha256Hex(data) {
		t.Errorf("got %q, want %q", got, sha256Hex(data))
	}
}

func TestVerifyImageChecksum_MatchingSidecar(t *testing.T) {
	data := []byte("trixie genericcloud")
	digest := sha256Hex(data)
	srv := fakeServer(t, data, digest)
	defer srv.Close()

	path := writeTempFile(t, data)
	if err := verifyImageChecksum(context.Background(), srv.URL+"/image", "", false, path); err != nil {
		t.Errorf("verifyImageChecksum error = %v", err)
	}
}

func TestVerifyImageChecksum_MismatchedSidecar(t *testing.T) {
	data := []byte("trixie genericcloud")
	wrong := strings.Repeat("0", 64)
	srv := fakeServer(t, data, wrong)
	defer srv.Close()

	path := writeTempFile(t, data)
	err := verifyImageChecksum(context.Background(), srv.URL+"/image", "", false, path)
	if err == nil {
		t.Fatal("expected mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("expected mismatch error, got %v", err)
	}
}

func TestVerifyImageChecksum_MissingSidecar_NonGitHub_Tolerant(t *testing.T) {
	// A user-supplied --image-url (strictSidecar=false) whose host doesn't
	// publish a per-image .sha256 sidecar (e.g. cloud.debian.org publishes
	// SHA256SUMS instead) must not block boot: a missing sidecar warns and
	// continues; only a mismatched sidecar is fatal.
	data := []byte("trixie genericcloud")
	srv := fakeServer(t, data, "404")
	defer srv.Close()

	path := writeTempFile(t, data)
	if err := verifyImageChecksum(context.Background(), srv.URL+"/image", "", false, path); err != nil {
		t.Fatalf("missing sidecar with strictSidecar=false should warn and pass; got: %v", err)
	}
}

// The hosted guest image (strictSidecar=true) always ships a published .sha256;
// a missing/404 sidecar must FAIL CLOSED (parity with the pinned Debian
// SHA-512), not warn-and-continue.
func TestVerifyImageChecksum_Hosted_MissingSidecar_FailsClosed(t *testing.T) {
	data := []byte("bladerunner guest image")
	srv := fakeServer(t, data, "404")
	defer srv.Close()

	path := writeTempFile(t, data)
	err := verifyImageChecksum(context.Background(), srv.URL+"/image", "", true, path)
	if err == nil {
		t.Fatal("expected a fatal error for a missing hosted sidecar, got nil")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("expected a 'missing' sidecar error, got %v", err)
	}
}

// A hosted sidecar that is unreachable (host down / connection refused) must
// also fail closed rather than boot unverified.
func TestVerifyImageChecksum_Hosted_UnreachableSidecar_FailsClosed(t *testing.T) {
	srv := fakeServer(t, []byte("guest image"), "unused")
	url := srv.URL + "/image"
	srv.Close() // close before the fetch so the sidecar request is refused

	path := writeTempFile(t, []byte("guest image"))
	err := verifyImageChecksum(context.Background(), url, "", true, path)
	if err == nil {
		t.Fatal("expected a fatal error for an unreachable hosted sidecar, got nil")
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("expected an 'unreachable' sidecar error, got %v", err)
	}
}

// A hosted image whose bytes don't match its published sidecar digest must fail
// closed on the mismatch.
func TestVerifyImageChecksum_Hosted_MismatchedSidecar_FailsClosed(t *testing.T) {
	data := []byte("bladerunner guest image")
	wrong := strings.Repeat("0", 64)
	srv := fakeServer(t, data, wrong)
	defer srv.Close()

	path := writeTempFile(t, data)
	err := verifyImageChecksum(context.Background(), srv.URL+"/image", "", true, path)
	if err == nil {
		t.Fatal("expected a mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("expected a 'mismatch' error, got %v", err)
	}
}

// A hosted image whose bytes match its published sidecar digest passes.
func TestVerifyImageChecksum_Hosted_MatchingSidecar(t *testing.T) {
	data := []byte("bladerunner guest image")
	digest := sha256Hex(data)
	srv := fakeServer(t, data, digest)
	defer srv.Close()

	path := writeTempFile(t, data)
	if err := verifyImageChecksum(context.Background(), srv.URL+"/image", "", true, path); err != nil {
		t.Errorf("verifyImageChecksum (hosted, matching sidecar) error = %v", err)
	}
}

func TestEnsureCachedBaseImage_DownloadVerifyAndHit(t *testing.T) {
	data := []byte("trixie genericcloud not-actually-qcow2")
	digest := sha256Hex(data)
	srv := fakeServer(t, data, "404")
	defer srv.Close()

	state := t.TempDir()
	t.Setenv("BLADERUNNER_STATE_DIR", state)

	cfg := &config.Config{
		BaseImageURL:            srv.URL + "/image",
		BaseImageExpectedSHA256: digest,
	}

	// Miss: downloads, verifies the pinned digest, and populates the cache.
	got, err := ensureBaseImage(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ensureBaseImage (miss): %v", err)
	}
	want := config.ImageCachePath(digest)
	if got != want {
		t.Fatalf("cache path = %q, want %q", got, want)
	}
	if !util.FileExists(want) || !util.FileExists(want+".ok") {
		t.Fatal("expected cache file and .ok stamp to be written")
	}

	// Hit: the same content-addressed path is returned without the server.
	srv.Close()
	got2, err := ensureBaseImage(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ensureBaseImage (hit): %v", err)
	}
	if got2 != want {
		t.Fatalf("hit path = %q, want %q", got2, want)
	}
}

func TestEnsureCachedBaseImage_DigestMismatch(t *testing.T) {
	data := []byte("trixie genericcloud")
	srv := fakeServer(t, data, "404")
	defer srv.Close()

	state := t.TempDir()
	t.Setenv("BLADERUNNER_STATE_DIR", state)

	wrong := strings.Repeat("0", 64)
	cfg := &config.Config{
		BaseImageURL:            srv.URL + "/image",
		BaseImageExpectedSHA256: wrong,
	}

	_, err := ensureBaseImage(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected SHA-256 mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("expected mismatch error, got %v", err)
	}
	// A mismatched download must not leave a usable cache entry.
	if util.FileExists(config.ImageCachePath(wrong) + ".ok") {
		t.Error("mismatched download must not write a .ok stamp")
	}
}

func TestVerifyImageChecksum_PinnedSHA512(t *testing.T) {
	data := []byte("trixie genericcloud pinned")
	sum := sha512.Sum512(data)
	want := hex.EncodeToString(sum[:])
	path := writeTempFile(t, data)

	// Matching embedded SHA-512: no network/sidecar needed, passes.
	if err := verifyImageChecksum(context.Background(), "http://example.invalid/image", want, false, path); err != nil {
		t.Errorf("verifyImageChecksum with matching pinned SHA-512 error = %v", err)
	}

	// Mismatched embedded SHA-512: fatal, and never touches the sidecar.
	wrong := strings.Repeat("a", 128)
	err := verifyImageChecksum(context.Background(), "http://example.invalid/image", wrong, false, path)
	if err == nil {
		t.Fatal("expected SHA-512 mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "SHA-512 mismatch") {
		t.Errorf("expected SHA-512 mismatch error, got %v", err)
	}
}

// hostedDebianServer serves a hosted image at /hosted (+ optional /hosted.sha256)
// and a Debian fallback at /debian, so the hosted->Debian auto-fallback can be
// exercised hermetically. hostedSidecar == "404" makes the hosted sidecar 404;
// "" omits the handler entirely (also a 404). The Debian image ships no sidecar
// and is verified via an embedded SHA-512 instead.
func hostedDebianServer(t *testing.T, hosted, hostedSidecar, debian []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/hosted", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(hosted) })
	if hostedSidecar != nil {
		mux.HandleFunc("/hosted.sha256", func(w http.ResponseWriter, _ *http.Request) {
			if string(hostedSidecar) == "404" {
				http.NotFound(w, nil)
				return
			}
			_, _ = w.Write(hostedSidecar)
		})
	}
	mux.HandleFunc("/debian", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(debian) })
	return httptest.NewServer(mux)
}

// withDebianFallback redirects the vm-package Debian fallback seam at the given
// URL + embedded SHA-512 for the duration of the test, so ensureHostedOrDebian
// falls back to a hermetic httptest endpoint instead of cloud.debian.org.
func withDebianFallback(t *testing.T, url string, data []byte) {
	t.Helper()
	sum := sha512.Sum512(data)
	sha := hex.EncodeToString(sum[:])
	prev := useDebianImage
	useDebianImage = func(cfg *config.Config) error {
		cfg.BaseImageURL = url
		cfg.BaseImageSHA512 = sha
		cfg.BaseImageExpectedSHA256 = ""
		cfg.BaseImagePath = ""
		cfg.UseHostedGuestImage = false
		return nil
	}
	t.Cleanup(func() { useDebianImage = prev })
}

// TestEnsureBaseImage_HostedSuccessStaysHosted verifies the default path: a
// hosted image with a matching fail-closed sidecar is used verbatim and cfg
// stays hosted (no fallback).
func TestEnsureBaseImage_HostedSuccessStaysHosted(t *testing.T) {
	hosted := []byte("pre-baked guest image bytes")
	sidecar := []byte(sha256Hex(hosted) + "\n")
	srv := hostedDebianServer(t, hosted, sidecar, []byte("debian bytes"))
	defer srv.Close()
	withDebianFallback(t, srv.URL+"/debian", []byte("debian bytes"))

	cfg := &config.Config{
		VMDir:               t.TempDir(),
		Arch:                "arm64",
		BaseImageURL:        srv.URL + "/hosted",
		UseHostedGuestImage: true,
	}
	got, err := ensureBaseImage(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ensureBaseImage: %v", err)
	}
	if !cfg.UseHostedGuestImage {
		t.Error("hosted success must not flip UseHostedGuestImage")
	}
	if data, _ := os.ReadFile(got); !bytes.Equal(data, hosted) {
		t.Errorf("booted image = %q, want the hosted bytes", string(data))
	}
}

// TestEnsureBaseImage_Hosted404FallsBackToDebian verifies a missing hosted asset
// (404 on the image) warns and lands on the verified Debian fallback — never an
// unverified image.
func TestEnsureBaseImage_Hosted404FallsBackToDebian(t *testing.T) {
	debian := []byte("debian genericcloud bytes")
	srv := hostedDebianServer(t, nil, nil, debian)
	srv.Close() // shut the hosted endpoint down entirely -> download error (like a 404/DNS fail)
	// Re-open only the debian endpoint on a fresh server.
	debSrv := hostedDebianServer(t, nil, nil, debian)
	defer debSrv.Close()
	withDebianFallback(t, debSrv.URL+"/debian", debian)

	cfg := &config.Config{
		VMDir:               t.TempDir(),
		Arch:                "arm64",
		BaseImageURL:        srv.URL + "/hosted", // dead server -> hosted download fails
		UseHostedGuestImage: true,
	}
	got, err := ensureBaseImage(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ensureBaseImage should have fallen back, got error: %v", err)
	}
	if cfg.UseHostedGuestImage {
		t.Error("fallback must disarm UseHostedGuestImage")
	}
	if data, _ := os.ReadFile(got); !bytes.Equal(data, debian) {
		t.Errorf("booted image = %q, want the Debian fallback bytes", string(data))
	}
	if cfg.BaseImageSHA512 == "" {
		t.Error("fallback must restore the pinned Debian SHA-512 (verified path)")
	}
}

// TestEnsureBaseImage_HostedChecksumMismatchFallsBackToDebian verifies the
// fail-closed sidecar: a hosted image whose sidecar does not match is rejected
// (never booted) and the run falls back to the verified Debian path.
func TestEnsureBaseImage_HostedChecksumMismatchFallsBackToDebian(t *testing.T) {
	hosted := []byte("corrupt-or-tampered hosted image")
	badSidecar := []byte(strings.Repeat("0", 64) + "\n") // valid-shaped hex, wrong digest
	debian := []byte("debian genericcloud bytes v2")
	srv := hostedDebianServer(t, hosted, badSidecar, debian)
	defer srv.Close()
	withDebianFallback(t, srv.URL+"/debian", debian)

	cfg := &config.Config{
		VMDir:               t.TempDir(),
		Arch:                "arm64",
		BaseImageURL:        srv.URL + "/hosted",
		UseHostedGuestImage: true,
	}
	got, err := ensureBaseImage(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ensureBaseImage should have fallen back on mismatch, got error: %v", err)
	}
	if cfg.UseHostedGuestImage {
		t.Error("checksum-mismatch fallback must disarm UseHostedGuestImage")
	}
	if data, _ := os.ReadFile(got); !bytes.Equal(data, debian) {
		t.Errorf("booted image = %q, want the Debian fallback bytes (never the mismatched hosted image)", string(data))
	}
}

// TestEnsureBaseImage_HostedMissingSidecarFallsBackToDebian verifies a missing
// (404) sidecar on the hosted image is fail-closed (never booted unverified) and
// the run falls back to the verified Debian path.
func TestEnsureBaseImage_HostedMissingSidecarFallsBackToDebian(t *testing.T) {
	hosted := []byte("hosted image with no published sidecar")
	debian := []byte("debian genericcloud bytes v3")
	srv := hostedDebianServer(t, hosted, []byte("404"), debian)
	defer srv.Close()
	withDebianFallback(t, srv.URL+"/debian", debian)

	cfg := &config.Config{
		VMDir:               t.TempDir(),
		Arch:                "arm64",
		BaseImageURL:        srv.URL + "/hosted",
		UseHostedGuestImage: true,
	}
	got, err := ensureBaseImage(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ensureBaseImage should have fallen back on missing sidecar, got error: %v", err)
	}
	if data, _ := os.ReadFile(got); !bytes.Equal(data, debian) {
		t.Errorf("booted image = %q, want the Debian fallback bytes", string(data))
	}
}

// --- G1: download retry classification -------------------------------------

// TestDownloadFile_RetriesTransient500ThenSucceeds verifies a 500 (transient) is
// retried with backoff and a subsequent 200 succeeds — a blip must not be fatal.
func TestDownloadFile_RetriesTransient500ThenSucceeds(t *testing.T) {
	body := []byte("recovered image bytes")
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "image.raw")
	if err := downloadFile(context.Background(), srv.URL, dst); err != nil {
		t.Fatalf("downloadFile should have retried past the 500, got: %v", err)
	}
	if n := atomic.LoadInt32(&calls); n < 2 {
		t.Fatalf("expected at least 2 attempts (retry), got %d", n)
	}
	if got, _ := os.ReadFile(dst); !bytes.Equal(got, body) {
		t.Errorf("downloaded bytes = %q, want %q", string(got), string(body))
	}
}

// TestDownloadFile_404NotRetried verifies a 404 (fatal) returns immediately
// without retrying, so the caller can fall back / fail fast.
func TestDownloadFile_404NotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.NotFound(w, nil)
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "image.raw")
	err := downloadFile(context.Background(), srv.URL, dst)
	if err == nil {
		t.Fatal("expected a fatal error for a 404, got nil")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("a 404 must not be retried: expected exactly 1 attempt, got %d", n)
	}
}

// TestTransientDownloadError_Classification exercises the classifier directly.
func TestTransientDownloadError_Classification(t *testing.T) {
	if transientDownloadError(&fatalHTTPError{status: "404 Not Found", code: 404}) {
		t.Error("a fatalHTTPError must be classified non-transient")
	}
	if transientDownloadError(context.Canceled) {
		t.Error("context.Canceled must be classified non-transient")
	}
	if !transientDownloadError(io.ErrUnexpectedEOF) {
		t.Error("io.ErrUnexpectedEOF must be classified transient")
	}
}

// --- G2: partial-artifact cleanup ------------------------------------------

// TestDownloadFile_FailedCopyRemovesTmp verifies a mid-stream body failure (the
// server closes the connection after promising more bytes via Content-Length)
// removes the partial ".tmp" instead of leaving a truncated artifact behind.
func TestDownloadFile_FailedCopyRemovesTmp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Promise more than we send, then hijack + close to force an unexpected EOF.
		w.Header().Set("Content-Length", "1048576")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("only a few bytes"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "image.raw")
	err := downloadFile(context.Background(), srv.URL, dst)
	if err == nil {
		t.Fatal("expected a mid-stream copy error, got nil")
	}
	if util.FileExists(dst + ".tmp") {
		t.Error("a failed download must remove the partial .tmp")
	}
	if util.FileExists(dst) {
		t.Error("a failed download must not leave the final artifact")
	}
}

// --- G3: disk-space preflight arithmetic -----------------------------------

func TestCheckDiskSpace(t *testing.T) {
	const gib = int64(1) << 30
	// Ample space: nil.
	if err := checkDiskSpace(10*gib, 64*gib, "/tmp/disk.raw"); err != nil {
		t.Errorf("expected nil when space is ample, got %v", err)
	}
	// Exactly enough: nil (avail == need is not "less than").
	if err := checkDiskSpace(64*gib, 64*gib, "/tmp/disk.raw"); err != nil {
		t.Errorf("expected nil when space is exactly enough, got %v", err)
	}
	// Insufficient: actionable error naming the path and the --disk hint.
	err := checkDiskSpace(64*gib, 10*gib, "/tmp/disk.raw")
	if err == nil {
		t.Fatal("expected an error when space is insufficient, got nil")
	}
	for _, want := range []string{"not enough disk space", "/tmp/disk.raw", "--disk"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must contain %q", err.Error(), want)
		}
	}
}

// --- Fix 1: sidecar fetch uses the bounded client + bounded retry ------------

// sidecarRetryServer serves /image.sha256 with a caller-controlled status
// sequence, counting hits so a test can assert retry (or no-retry) behavior.
func sidecarRetryServer(t *testing.T, hits *int32, statuses []int, body string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/image.sha256", func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(hits, 1)
		idx := int(n) - 1
		if idx >= len(statuses) {
			idx = len(statuses) - 1
		}
		code := statuses[idx]
		if code != http.StatusOK {
			http.Error(w, http.StatusText(code), code)
			return
		}
		_, _ = w.Write([]byte(body))
	})
	return httptest.NewServer(mux)
}

// TestFetchSidecarSHA256_RetriesTransient500ThenSucceeds verifies a transient
// 500 on the FIRST sidecar fetch is retried with backoff and a subsequent 200
// yields the digest — a blip must not fail the fetch.
func TestFetchSidecarSHA256_RetriesTransient500ThenSucceeds(t *testing.T) {
	digest := strings.Repeat("a", 64)
	var hits int32
	srv := sidecarRetryServer(t, &hits, []int{http.StatusInternalServerError, http.StatusOK}, digest+"\n")
	defer srv.Close()

	got, err := fetchSidecarSHA256(context.Background(), srv.URL+"/image")
	if err != nil {
		t.Fatalf("fetchSidecarSHA256 should have retried past the 500, got: %v", err)
	}
	if got != digest {
		t.Errorf("digest = %q, want %q", got, digest)
	}
	if n := atomic.LoadInt32(&hits); n < 2 {
		t.Fatalf("expected at least 2 attempts (retry), got %d", n)
	}
}

// TestFetchSidecarSHA256_404NotRetried verifies a 404 (no published sidecar) is
// NOT retried: it returns ("", nil) immediately so the caller decides, and the
// server is hit exactly once.
func TestFetchSidecarSHA256_404NotRetried(t *testing.T) {
	var hits int32
	srv := sidecarRetryServer(t, &hits, []int{http.StatusNotFound}, "")
	defer srv.Close()

	got, err := fetchSidecarSHA256(context.Background(), srv.URL+"/image")
	if err != nil {
		t.Fatalf("a 404 sidecar must return no error, got %v", err)
	}
	if got != "" {
		t.Errorf("a 404 sidecar must return an empty digest, got %q", got)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("a 404 sidecar must not be retried: expected exactly 1 hit, got %d", n)
	}
}

// TestFetchSidecarSHA256_403NotRetried verifies a definitive 403 (a non-404
// terminal 4xx) fails fast without retrying rather than burning backoff.
func TestFetchSidecarSHA256_403NotRetried(t *testing.T) {
	var hits int32
	srv := sidecarRetryServer(t, &hits, []int{http.StatusForbidden}, "")
	defer srv.Close()

	if _, err := fetchSidecarSHA256(context.Background(), srv.URL+"/image"); err == nil {
		t.Fatal("expected a terminal error for a 403 sidecar, got nil")
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("a 403 sidecar must not be retried: expected exactly 1 hit, got %d", n)
	}
}

// TestFetchSidecarSHA256_UsesBoundedClient asserts the fetch is issued over the
// package's bounded downloadClient (dial/TLS/idle timeouts), not
// http.DefaultClient — the whole point of Fix 1 (a captive portal / silent TCP
// drop must not hang boot forever). The RoundTripper is temporarily swapped to
// observe that the sidecar request flows through downloadClient's transport.
func TestFetchSidecarSHA256_UsesBoundedClient(t *testing.T) {
	digest := strings.Repeat("c", 64)
	srv := fakeServer(t, nil, digest+"\n")
	defer srv.Close()

	base := downloadClient.Transport
	var used atomic.Bool
	downloadClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/image.sha256") {
			used.Store(true)
		}
		return base.RoundTrip(r)
	})
	t.Cleanup(func() { downloadClient.Transport = base })

	if _, err := fetchSidecarSHA256(context.Background(), srv.URL+"/image"); err != nil {
		t.Fatalf("fetchSidecarSHA256 error = %v", err)
	}
	if !used.Load() {
		t.Error("sidecar fetch must go through the bounded downloadClient, not http.DefaultClient")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// --- Fix 2: custom --image-url / --image-path fail CLOSED --------------------

// TestEnsureBaseImage_CustomURL_NoShaNoSidecar_FailsClosed verifies a custom
// --image-url with neither an explicit --image-sha256 nor a published .sha256
// sidecar REFUSES to boot (fail-closed) instead of warning and booting an
// unverified image.
func TestEnsureBaseImage_CustomURL_NoShaNoSidecar_FailsClosed(t *testing.T) {
	data := []byte("unverified custom image bytes")
	srv := fakeServer(t, data, "404") // no sidecar published
	defer srv.Close()

	prev := requireVerifiedCustomImage
	SetRequireVerifiedCustomImage(true)
	t.Cleanup(func() { requireVerifiedCustomImage = prev })

	cfg := &config.Config{
		VMDir:        t.TempDir(),
		Arch:         "arm64",
		BaseImageURL: srv.URL + "/image",
	}
	_, err := ensureBaseImage(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected a fail-closed error for an unverified custom image, got nil")
	}
	if !strings.Contains(err.Error(), "--image-sha256") || !strings.Contains(err.Error(), "sidecar") {
		t.Errorf("error must name the remedy (--image-sha256 / sidecar), got: %v", err)
	}
}

// TestEnsureBaseImage_CustomURL_MatchingSidecar_OK verifies a custom --image-url
// with a present+matching .sha256 sidecar boots (verified).
func TestEnsureBaseImage_CustomURL_MatchingSidecar_OK(t *testing.T) {
	data := []byte("verified custom image bytes")
	srv := fakeServer(t, data, sha256Hex(data)+"\n")
	defer srv.Close()

	prev := requireVerifiedCustomImage
	SetRequireVerifiedCustomImage(true)
	t.Cleanup(func() { requireVerifiedCustomImage = prev })

	cfg := &config.Config{
		VMDir:        t.TempDir(),
		Arch:         "arm64",
		BaseImageURL: srv.URL + "/image",
	}
	got, err := ensureBaseImage(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ensureBaseImage with a matching sidecar should pass, got: %v", err)
	}
	if b, _ := os.ReadFile(got); !bytes.Equal(b, data) {
		t.Errorf("booted image = %q, want the custom bytes", string(b))
	}
}

// TestEnsureBaseImage_CustomURL_MismatchedSidecar_FailsClosed verifies a custom
// --image-url whose bytes don't match its published sidecar digest fails closed.
func TestEnsureBaseImage_CustomURL_MismatchedSidecar_FailsClosed(t *testing.T) {
	data := []byte("tampered custom image bytes")
	srv := fakeServer(t, data, strings.Repeat("0", 64)+"\n") // valid-shaped, wrong digest
	defer srv.Close()

	prev := requireVerifiedCustomImage
	SetRequireVerifiedCustomImage(true)
	t.Cleanup(func() { requireVerifiedCustomImage = prev })

	cfg := &config.Config{
		VMDir:        t.TempDir(),
		Arch:         "arm64",
		BaseImageURL: srv.URL + "/image",
	}
	_, err := ensureBaseImage(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected a mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("expected a mismatch error, got %v", err)
	}
}

// TestEnsureBaseImage_CustomURL_ExplicitSHA256_Matches verifies a custom
// --image-url pinned with an explicit --image-sha256 (threaded via
// BaseImageExpectedSHA256) is verified fail-closed against that digest and
// boots when it matches — no sidecar required.
func TestEnsureBaseImage_CustomURL_ExplicitSHA256_Matches(t *testing.T) {
	data := []byte("explicitly pinned custom image bytes")
	digest := sha256Hex(data)
	srv := fakeServer(t, data, "404") // no sidecar; the explicit hash is authoritative
	defer srv.Close()

	state := t.TempDir()
	t.Setenv("BLADERUNNER_STATE_DIR", state)

	cfg := &config.Config{
		VMDir:                   t.TempDir(),
		Arch:                    "arm64",
		BaseImageURL:            srv.URL + "/image",
		BaseImageExpectedSHA256: digest,
	}
	got, err := ensureBaseImage(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ensureBaseImage with a matching explicit --image-sha256 should pass, got: %v", err)
	}
	if got != config.ImageCachePath(digest) {
		t.Errorf("explicit-hash image should resolve via the content-addressed cache, got %q", got)
	}
}

// TestEnsureBaseImage_CustomURL_ExplicitSHA256_Mismatch verifies a custom
// --image-url pinned with an explicit --image-sha256 fails closed on mismatch.
func TestEnsureBaseImage_CustomURL_ExplicitSHA256_Mismatch(t *testing.T) {
	data := []byte("bytes that will not match the pin")
	srv := fakeServer(t, data, "404")
	defer srv.Close()

	state := t.TempDir()
	t.Setenv("BLADERUNNER_STATE_DIR", state)

	cfg := &config.Config{
		VMDir:                   t.TempDir(),
		Arch:                    "arm64",
		BaseImageURL:            srv.URL + "/image",
		BaseImageExpectedSHA256: strings.Repeat("0", 64),
	}
	_, err := ensureBaseImage(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected a mismatch error for a wrong --image-sha256, got nil")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("expected a mismatch error, got %v", err)
	}
}

// TestEnsureBaseImage_ImagePath_NoHash_OK verifies a local --image-path with NO
// --image-path-sha256 is usable without a hash (the user controls the file).
func TestEnsureBaseImage_ImagePath_NoHash_OK(t *testing.T) {
	data := []byte("local image, not a qcow2 header")
	path := writeTempFile(t, data)

	cfg := &config.Config{BaseImagePath: path, Arch: "arm64"}
	got, err := ensureBaseImage(context.Background(), cfg)
	if err != nil {
		t.Fatalf("a local --image-path without a hash should be usable, got: %v", err)
	}
	if got != path {
		t.Errorf("image path = %q, want %q", got, path)
	}
}

// TestEnsureBaseImage_ImagePath_Hash_Matches verifies a local --image-path with
// a matching --image-path-sha256 (threaded via BaseImageExpectedSHA256) passes.
func TestEnsureBaseImage_ImagePath_Hash_Matches(t *testing.T) {
	data := []byte("local image with a pinned hash")
	path := writeTempFile(t, data)

	cfg := &config.Config{
		BaseImagePath:           path,
		Arch:                    "arm64",
		BaseImageExpectedSHA256: sha256Hex(data),
	}
	got, err := ensureBaseImage(context.Background(), cfg)
	if err != nil {
		t.Fatalf("a matching --image-path-sha256 should pass, got: %v", err)
	}
	if got != path {
		t.Errorf("image path = %q, want %q", got, path)
	}
}

// TestEnsureBaseImage_ImagePath_Hash_Mismatch verifies a local --image-path with
// a mismatched --image-path-sha256 fails closed.
func TestEnsureBaseImage_ImagePath_Hash_Mismatch(t *testing.T) {
	data := []byte("local image whose hash will not match")
	path := writeTempFile(t, data)

	cfg := &config.Config{
		BaseImagePath:           path,
		Arch:                    "arm64",
		BaseImageExpectedSHA256: strings.Repeat("0", 64),
	}
	_, err := ensureBaseImage(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected a mismatch error for a wrong --image-path-sha256, got nil")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("expected a mismatch error, got %v", err)
	}
}
