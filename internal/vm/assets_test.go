package vm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
