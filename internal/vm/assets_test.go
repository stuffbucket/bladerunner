package vm

import (
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

// A mismatched sidecar is fatal regardless of strictness — a corrupt or tampered
// pre-baked image must never be accepted (it triggers the Debian fallback).
func TestVerifyImageChecksum_MismatchedSidecar_Strict(t *testing.T) {
	data := []byte("guest image")
	wrong := strings.Repeat("0", 64)
	srv := fakeServer(t, data, wrong)
	defer srv.Close()

	path := writeTempFile(t, data)
	err := verifyImageChecksum(context.Background(), srv.URL+"/image", "", true, path)
	if err == nil {
		t.Fatal("expected strict mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("expected mismatch error, got %v", err)
	}
}

func TestVerifyImageChecksum_MissingSidecar_NonStrict_Tolerant(t *testing.T) {
	// A custom --image-url (non-strict) whose host publishes no per-image
	// .sha256 sidecar (e.g. cloud.debian.org publishes SHA256SUMS instead)
	// must not block boot: missing sidecars warn and continue when non-strict.
	data := []byte("trixie genericcloud")
	srv := fakeServer(t, data, "404")
	defer srv.Close()

	path := writeTempFile(t, data)
	if err := verifyImageChecksum(context.Background(), srv.URL+"/image", "", false, path); err != nil {
		t.Fatalf("missing sidecar (non-strict) should warn and pass; got: %v", err)
	}
}

// The pre-baked hosted default is strict: a missing sidecar is FATAL so the
// default fails closed on an unverifiable image (the caller then falls back to
// Debian rather than booting it).
func TestVerifyImageChecksum_MissingSidecar_Strict_FailsClosed(t *testing.T) {
	data := []byte("guest image")
	srv := fakeServer(t, data, "404")
	defer srv.Close()

	path := writeTempFile(t, data)
	err := verifyImageChecksum(context.Background(), srv.URL+"/image", "", true, path)
	if err == nil {
		t.Fatal("strict missing-sidecar should fail closed, got nil")
	}
	if !strings.Contains(err.Error(), "not published") {
		t.Errorf("expected 'not published' error, got %v", err)
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

// twoImageServer serves a "hosted" image + sidecar and a "debian" image +
// sidecar under distinct paths so a single httptest server can model both the
// pre-baked default and its fallback. A "404" sidecar returns Not Found.
func twoImageServer(t *testing.T, hosted, hostedSidecar, debian, debianSidecar []byte, debianSidecar404 bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/hosted", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(hosted) })
	mux.HandleFunc("/hosted.sha256", func(w http.ResponseWriter, _ *http.Request) {
		if hostedSidecar == nil {
			http.NotFound(w, nil)
			return
		}
		_, _ = w.Write(hostedSidecar)
	})
	mux.HandleFunc("/debian", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(debian) })
	mux.HandleFunc("/debian.sha256", func(w http.ResponseWriter, _ *http.Request) {
		if debianSidecar404 {
			http.NotFound(w, nil)
			return
		}
		_, _ = w.Write(debianSidecar)
	})
	return httptest.NewServer(mux)
}

// withDebianFallback redirects the package-level Debian fallback resolvers to a
// fixed URL/SHA for the duration of a test, so the hosted->Debian fallback can be
// exercised against a local server. debSHA512 == "" makes the fallback use
// non-strict sidecar verification (matching an unknown/test arch).
func withDebianFallback(t *testing.T, debURL, debSHA512 string) {
	t.Helper()
	origURL, origSHA := debianFallbackURL, debianFallbackSHA512
	debianFallbackURL = func(string) (string, error) { return debURL, nil }
	debianFallbackSHA512 = func(string) string { return debSHA512 }
	t.Cleanup(func() { debianFallbackURL, debianFallbackSHA512 = origURL, origSHA })
}

// TestEnsureBaseImage_HostedDefault_Verified: the happy default path — the hosted
// image downloads, its sidecar matches, and the pre-baked+agent path is kept.
func TestEnsureBaseImage_HostedDefault_Verified(t *testing.T) {
	hosted := []byte("pre-baked guest image bytes")
	srv := twoImageServer(t, hosted, []byte(sha256Hex(hosted)), nil, nil, true)
	defer srv.Close()

	cfg := &config.Config{
		VMDir:               t.TempDir(),
		Arch:                "arm64",
		BaseImageURL:        srv.URL + "/hosted",
		HostedImageFallback: true,
		UseHostedGuestImage: true,
		UseGuestAgent:       true,
	}
	got, err := ensureBaseImage(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ensureBaseImage: %v", err)
	}
	if !util.FileExists(got) {
		t.Fatalf("expected image at %q", got)
	}
	if !cfg.UseHostedGuestImage || !cfg.UseGuestAgent {
		t.Errorf("verified hosted image must keep the agent path: hosted=%v agent=%v",
			cfg.UseHostedGuestImage, cfg.UseGuestAgent)
	}
}

// TestEnsureBaseImage_HostedMissingSidecar_FallsBack: the pre-baked sidecar is
// absent (unverifiable) -> fail closed on the hosted image and auto-fall-back to
// the Debian + cloud-init path, flipping the provisioning selectors.
func TestEnsureBaseImage_HostedMissingSidecar_FallsBack(t *testing.T) {
	hosted := []byte("pre-baked guest image bytes")
	debian := []byte("debian genericcloud bytes")
	// hostedSidecar=nil -> 404 (missing); debian served with a matching sidecar.
	srv := twoImageServer(t, hosted, nil, debian, []byte(sha256Hex(debian)), false)
	defer srv.Close()
	withDebianFallback(t, srv.URL+"/debian", "") // "" -> non-strict sidecar verify

	cfg := &config.Config{
		VMDir:               t.TempDir(),
		Arch:                "arm64",
		BaseImageURL:        srv.URL + "/hosted",
		HostedImageFallback: true,
		UseHostedGuestImage: true,
		UseGuestAgent:       true,
	}
	got, err := ensureBaseImage(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ensureBaseImage (fallback) unexpected error: %v", err)
	}
	if !util.FileExists(got) {
		t.Fatalf("expected fallback image at %q", got)
	}
	// The fallback must have flipped the config to the Debian + cloud-init path.
	if cfg.UseHostedGuestImage {
		t.Error("fallback must clear UseHostedGuestImage")
	}
	if cfg.UseGuestAgent {
		t.Error("fallback must clear UseGuestAgent (cloud-init path)")
	}
	if cfg.HostedImageFallback {
		t.Error("fallback must clear HostedImageFallback")
	}
	if cfg.BaseImageURL != srv.URL+"/debian" {
		t.Errorf("BaseImageURL after fallback = %q, want the Debian URL", cfg.BaseImageURL)
	}
}

// TestEnsureBaseImage_HostedMismatch_FallsBack: the pre-baked sidecar is present
// but WRONG (corrupt/tampered) -> reject the hosted image (fail closed) and fall
// back rather than booting an image that failed verification.
func TestEnsureBaseImage_HostedMismatch_FallsBack(t *testing.T) {
	hosted := []byte("pre-baked guest image bytes")
	debian := []byte("debian genericcloud bytes")
	wrong := []byte(strings.Repeat("0", 64)) // valid-shaped but wrong digest
	srv := twoImageServer(t, hosted, wrong, debian, []byte(sha256Hex(debian)), false)
	defer srv.Close()
	withDebianFallback(t, srv.URL+"/debian", "")

	cfg := &config.Config{
		VMDir:               t.TempDir(),
		Arch:                "arm64",
		BaseImageURL:        srv.URL + "/hosted",
		HostedImageFallback: true,
		UseHostedGuestImage: true,
		UseGuestAgent:       true,
	}
	got, err := ensureBaseImage(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ensureBaseImage (mismatch->fallback) unexpected error: %v", err)
	}
	if cfg.UseHostedGuestImage || cfg.UseGuestAgent {
		t.Error("a mismatched hosted image must fall back to the cloud-init path")
	}
	if cfg.BaseImageURL != srv.URL+"/debian" {
		t.Errorf("BaseImageURL after mismatch fallback = %q, want the Debian URL", cfg.BaseImageURL)
	}
	if !util.FileExists(got) {
		t.Fatalf("expected fallback image at %q", got)
	}
}

// TestEnsureBaseImage_HostedFails_NoDebianForArch: when the hosted image fails
// and no Debian fallback exists for the arch, ensureBaseImage surfaces the
// original hosted failure (never a silent success).
func TestEnsureBaseImage_HostedFails_NoDebianForArch(t *testing.T) {
	hosted := []byte("pre-baked guest image bytes")
	srv := twoImageServer(t, hosted, nil, nil, nil, true) // hosted sidecar 404
	defer srv.Close()
	// Real resolver: an unsupported arch has no Debian image.
	cfg := &config.Config{
		VMDir:               t.TempDir(),
		Arch:                "riscv64",
		BaseImageURL:        srv.URL + "/hosted",
		HostedImageFallback: true,
		UseHostedGuestImage: true,
		UseGuestAgent:       true,
	}
	_, err := ensureBaseImage(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error when hosted fails and no Debian fallback exists")
	}
	if !strings.Contains(err.Error(), "no Debian fallback") {
		t.Errorf("expected 'no Debian fallback' error, got %v", err)
	}
}
