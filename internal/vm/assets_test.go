package vm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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
	got, err := util.FileSHA256(path)
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

// The DEFAULT hosted path must use the shared cache, so a second instance
// reuses the ~1 GB the first one already downloaded and converted.
//
// It did not. Only a disk manifest's pinned digest reached the cache; the
// default wrote base-image.raw into each instance's own directory, so a machine
// with two instances held two copies of identical bytes while the shared cache
// sat empty. Measured on a real first boot, that duplicate cost 43s of download
// and ~1 GB of disk per instance.
func TestHostedImageIsSharedAcrossInstances(t *testing.T) {
	data := []byte("pre-baked guest image, not actually a qcow2")
	digest := sha256Hex(data)
	srv := fakeServer(t, data, digest)
	defer srv.Close()

	state := t.TempDir()
	t.Setenv("BLADERUNNER_STATE_DIR", state)

	// Two instances, each with its own VM directory, as `br start --state-dir`
	// and every disk slot produce.
	first := &config.Config{
		VMDir:               t.TempDir(),
		BaseImageURL:        srv.URL + "/image",
		UseHostedGuestImage: true,
	}
	second := &config.Config{
		VMDir:               t.TempDir(),
		BaseImageURL:        srv.URL + "/image",
		UseHostedGuestImage: true,
	}

	got, err := ensureBaseImage(context.Background(), first)
	if err != nil {
		t.Fatalf("first instance: %v", err)
	}
	want := config.ImageCachePath(digest)
	if got != want {
		t.Fatalf("first instance resolved %q, want the shared cache %q", got, want)
	}
	if util.FileExists(filepath.Join(first.VMDir, "base-image.raw")) {
		t.Error("the first instance still wrote a private base-image.raw beside the shared copy")
	}

	// The server is gone. A second instance can only succeed from the cache,
	// which is the whole point.
	srv.Close()
	got2, err := ensureBaseImage(context.Background(), second)
	if err != nil {
		t.Fatalf("second instance could not reuse the cached image: %v", err)
	}
	if got2 != want {
		t.Errorf("second instance resolved %q, want the shared cache %q", got2, want)
	}
	if util.FileExists(filepath.Join(second.VMDir, "base-image.raw")) {
		t.Error("the second instance downloaded its own copy despite a populated cache")
	}
}

// An unverifiable hosted image must still fall back rather than boot.
//
// Fetching the sidecar earlier changes WHEN the digest is learned, not whether
// it is required. A missing sidecar has to reach the same Debian fallback it
// always did, or this change would have quietly turned a fail-closed path into
// a fail-open one.
func TestHostedCacheStillFailsClosedWithoutASidecar(t *testing.T) {
	data := []byte("hosted image with no sidecar")
	debian := []byte("debian genericcloud fallback")
	srv := hostedDebianServer(t, data, nil, debian)
	defer srv.Close()

	t.Setenv("BLADERUNNER_STATE_DIR", t.TempDir())

	cfg := &config.Config{
		VMDir:               t.TempDir(),
		BaseImageURL:        srv.URL + "/hosted",
		UseHostedGuestImage: true,
		Arch:                "arm64",
	}

	got, err := ensureBaseImage(context.Background(), cfg)
	if err == nil && got == config.ImageCachePath(sha256Hex(data)) {
		t.Fatal("an unverified hosted image was cached and used; the sidecar must remain mandatory")
	}
}

// A pointer to bytes that are gone, or to bytes whose verification never
// finished, is not a cache hit.
//
// The offline path trusts the .ok stamp as the record that these bytes were
// verified when stored. If a hit could be produced without it, an interrupted
// download would become a bootable image.
func TestCachedImageForURLRequiresBothFileAndStamp(t *testing.T) {
	t.Setenv("BLADERUNNER_STATE_DIR", t.TempDir())
	const url = "https://example.invalid/guest.qcow2"
	digest := sha256Hex([]byte("some bytes"))

	if _, ok := cachedImageForURL(url); ok {
		t.Fatal("reported a hit with no pointer written")
	}

	rememberImageForURL(url, digest)
	if _, ok := cachedImageForURL(url); ok {
		t.Error("reported a hit for a pointer whose image file does not exist")
	}

	cachePath := config.ImageCachePath(digest)
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	if err := os.WriteFile(cachePath, []byte("some bytes"), 0o600); err != nil {
		t.Fatalf("write cache file: %v", err)
	}
	if _, ok := cachedImageForURL(url); ok {
		t.Error("reported a hit for an image with no .ok stamp; verification may never have completed")
	}

	if err := os.WriteFile(cachePath+".ok", nil, 0o600); err != nil {
		t.Fatalf("write stamp: %v", err)
	}
	got, ok := cachedImageForURL(url)
	if !ok {
		t.Fatal("no hit once both the image and its verification stamp exist")
	}
	if got != cachePath {
		t.Errorf("hit resolved to %q, want %q", got, cachePath)
	}
}

// --- the shared cache under concurrent cold boots -------------------------

const (
	// racingChunkSize is the body chunk the racing server flushes at a time.
	// The body is served in pieces so two downloads are inside io.Copy
	// together rather than one finishing before the other opens its file.
	racingChunkSize = 128 * 1024
	// racingChunks makes the served body big enough for that overlap to be
	// wide, without making the test slow.
	racingChunks = 64
	// racingBarrierGrace releases the two-request barrier when only ONE
	// request ever arrives, which is exactly what a build that serializes the
	// cache does. It is a release valve for the serialized path, never the
	// synchronization for the racing one: two concurrent downloads trip the
	// barrier on the request COUNT and never wait for this timer.
	racingBarrierGrace = 250 * time.Millisecond
)

// racingImageServer serves image bytes at /image (with a 404 sidecar) and holds
// every request until two are in flight, so two concurrent
// ensureCachedBaseImage calls stage their downloads at the same instant. The
// returned func reports how many image downloads were served.
func racingImageServer(t *testing.T, image []byte) (*httptest.Server, func() int) {
	t.Helper()

	var mu sync.Mutex
	requests := 0
	release := make(chan struct{})
	var releaseOnce sync.Once
	open := func() { releaseOnce.Do(func() { close(release) }) }

	mux := http.NewServeMux()
	mux.HandleFunc("/image.sha256", func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) })
	mux.HandleFunc("/image", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		requests++
		inFlight := requests
		mu.Unlock()
		if inFlight >= 2 {
			open()
		}
		select {
		case <-release:
		case <-time.After(racingBarrierGrace):
			open()
		}

		w.Header().Set("Content-Length", strconv.Itoa(len(image)))
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for off := 0; off < len(image); off += racingChunkSize {
			if _, err := w.Write(image[off:min(off+racingChunkSize, len(image))]); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	})

	srv := httptest.NewServer(mux)
	return srv, func() int {
		mu.Lock()
		defer mu.Unlock()
		return requests
	}
}

// TestEnsureCachedBaseImage_ConcurrentColdBoots holds the exclusion the shared
// image cache needs. Multi-instance makes two cold boots at the same moment an
// ordinary user action, and the cache is global while every other lock in the
// system is per-state-directory. Without exclusion both callers stage through
// the same fixed path, the bytes interleave, and both fail with a digest
// mismatch that reads like a tampered upstream artifact.
func TestEnsureCachedBaseImage_ConcurrentColdBoots(t *testing.T) {
	data := bytes.Repeat([]byte("bladerunner shared base image bytes\n"), racingChunks*racingChunkSize/36)
	digest := sha256Hex(data)
	srv, served := racingImageServer(t, data)
	defer srv.Close()

	t.Setenv("BLADERUNNER_STATE_DIR", t.TempDir())
	cfg := &config.Config{
		BaseImageURL:            srv.URL + "/image",
		BaseImageExpectedSHA256: digest,
	}

	const callers = 2
	paths := make([]string, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			paths[i], errs[i] = ensureCachedBaseImage(context.Background(), cfg, digest)
		}()
	}
	wg.Wait()

	want := config.ImageCachePath(digest)
	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: ensureCachedBaseImage: %v", i, errs[i])
		}
		if paths[i] != want {
			t.Errorf("caller %d resolved to %q, want %q", i, paths[i], want)
		}
	}

	cached, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("read cached image: %v", err)
	}
	if !bytes.Equal(cached, data) {
		t.Fatalf("cached image is %d bytes and does not match the served image (%d bytes)", len(cached), len(data))
	}
	if !util.FileExists(want + ".ok") {
		t.Error("a completed cache entry must carry its .ok stamp")
	}
	if n := served(); n != 1 {
		t.Errorf("served %d downloads, want 1: the waiting caller must reuse the first download, not repeat it", n)
	}
}

// TestEnsureCachedBaseImage_BusyCacheIsNotAMismatch holds the diagnostic half
// of the same defect: a failure caused by LOCAL concurrency must never be
// worded like a genuine digest mismatch, which sends the user hunting a
// supply-chain compromise that did not happen.
func TestEnsureCachedBaseImage_BusyCacheIsNotAMismatch(t *testing.T) {
	data := []byte("staged by another process")
	digest := sha256Hex(data)
	srv := fakeServer(t, data, "404")
	defer srv.Close()

	t.Setenv("BLADERUNNER_STATE_DIR", t.TempDir())
	if err := os.MkdirAll(config.ImageCacheDir(), 0o755); err != nil {
		t.Fatalf("create cache dir: %v", err)
	}
	held, err := lockImageCacheEntry(context.Background(), config.ImageCachePath(digest))
	if err != nil {
		t.Fatalf("take the cache lock: %v", err)
	}
	defer held.release()

	// A canceled context stands in for "gave up waiting", with no sleep.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cfg := &config.Config{
		BaseImageURL:            srv.URL + "/image",
		BaseImageExpectedSHA256: digest,
	}
	_, err = ensureCachedBaseImage(ctx, cfg, digest)
	if err == nil {
		t.Fatal("expected a busy-cache error while another process holds the entry")
	}
	if !errors.Is(err, errImageCacheBusy) {
		t.Errorf("error is not errImageCacheBusy: %v", err)
	}
	if strings.Contains(err.Error(), "mismatch") {
		t.Errorf("a local concurrency failure must not read as a digest mismatch: %v", err)
	}
}

// --- conversion safety ----------------------------------------------------

// qemuImgStub puts a fake qemu-img first on PATH for the duration of the test,
// so a conversion failure can be produced without qemu installed and without
// writing a multi-gigabyte image.
func qemuImgStub(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "qemu-img"), []byte(script), 0o755); err != nil {
		t.Fatalf("write qemu-img stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// qcow2Fixture writes a file with the qcow2 magic, which is what makes
// ensureRawDiskImage convert it.
func qcow2Fixture(t *testing.T) (path string, content []byte) {
	t.Helper()
	content = []byte("QFI\xfbthe only copy of the user's disk image")
	return writeTempFile(t, content), content
}

// pathExists reports whether anything at all is at path — a file OR a
// directory, unlike util.FileExists.
func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// TestConvertQcow2ToRaw_FailureRemovesPartialOutput holds that a conversion
// that fails partway reclaims what qemu-img had already written. The raw
// expansion of a ~1 GB image is several GB, a full disk is the likeliest
// trigger, and a leaked partial makes the retry more likely to fail the same
// way — a ratchet.
func TestConvertQcow2ToRaw_FailureRemovesPartialOutput(t *testing.T) {
	qemuImgStub(t, "#!/bin/sh\nfor a in \"$@\"; do dst=\"$a\"; done\nprintf 'partially converted' > \"$dst\"\necho 'simulated qemu-img failure' >&2\nexit 1\n")
	path, content := qcow2Fixture(t)

	if err := ensureRawDiskImage(context.Background(), path); err == nil {
		t.Fatal("expected the stubbed conversion to fail")
	}
	if pathExists(path + ".raw") {
		t.Error("a failed conversion must not leave its partial output on disk")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read input after the failed conversion: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Error("a failed conversion must leave its input byte-identical")
	}
}

// TestConvertQcow2ToRaw_FailedRenameKeepsInput holds the data-safety half.
// ensureBaseImage reaches this function with a CALLER-OWNED path — the user's
// --base-image-path — so unlinking the source before the rename can destroy a
// file bladerunner does not own and cannot re-fetch. The stub reports success
// without producing an output file, which makes the rename fail; it stands for
// any rename failure (a crash in the window, EIO, a read-only directory). The
// input must still be there afterward.
func TestConvertQcow2ToRaw_FailedRenameKeepsInput(t *testing.T) {
	qemuImgStub(t, "#!/bin/sh\nexit 0\n")
	path, content := qcow2Fixture(t)

	if err := ensureRawDiskImage(context.Background(), path); err == nil {
		t.Fatal("expected the rename onto the input to fail")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the caller's input is gone after a failed rename: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Error("a failed rename must leave the caller's input byte-identical")
	}
	if pathExists(path + ".raw") {
		t.Error("a failed conversion must not leave its output on disk")
	}
}
