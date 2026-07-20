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
	if err := verifyImageChecksum(context.Background(), srv.URL+"/image", "", path); err != nil {
		t.Errorf("verifyImageChecksum error = %v", err)
	}
}

func TestVerifyImageChecksum_MismatchedSidecar(t *testing.T) {
	data := []byte("trixie genericcloud")
	wrong := strings.Repeat("0", 64)
	srv := fakeServer(t, data, wrong)
	defer srv.Close()

	path := writeTempFile(t, data)
	err := verifyImageChecksum(context.Background(), srv.URL+"/image", "", path)
	if err == nil {
		t.Fatal("expected mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("expected mismatch error, got %v", err)
	}
}

func TestVerifyImageChecksum_MissingSidecar_NonGitHub_Tolerant(t *testing.T) {
	// Upstream image hosts that don't publish per-image .sha256 sidecars
	// (e.g. cloud.debian.org publishes SHA256SUMS instead) must not block
	// boot. Missing sidecars warn and continue universally; only mismatched
	// sidecars are fatal.
	data := []byte("trixie genericcloud")
	srv := fakeServer(t, data, "404")
	defer srv.Close()

	path := writeTempFile(t, data)
	if err := verifyImageChecksum(context.Background(), srv.URL+"/image", "", path); err != nil {
		t.Fatalf("missing sidecar on non-GitHub URL should warn and pass; got: %v", err)
	}
}

func TestVerifyImageChecksum_MissingSidecar_GitHub_Tolerant(t *testing.T) {
	data := []byte("guest image")
	// Use a mock URL that looks like a GitHub release. The httptest server
	// path won't match github.com, so simulate via a custom handler proxy.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Craft a URL that contains "github.com" and "/releases/" so the
	// tolerance branch fires; but the actual HTTP fetch goes to srv.
	// Easiest: bypass and unit-test the tolerance branch directly by
	// calling verifyImageChecksum with a URL whose sidecar 404s but
	// passes isGitHubReleaseURL. We achieve that by constructing the
	// URL string to *contain* the markers; networking happens against
	// the test server because of the host prefix.
	//
	// Instead, validate the branch by checking isGitHubReleaseURL is
	// the gate, and exercise the no-op path via a 404 sidecar at a
	// URL that satisfies the heuristic.
	url := srv.URL + "/github.com/foo/releases/download/x/image"
	path := writeTempFile(t, data)
	// The sidecar URL will be srv.URL + "/github.com/foo/releases/download/x/image.sha256"
	// which our mux 404s. Because url contains "github.com/" and "/releases/",
	// the function should treat the missing sidecar as a warning, not error.
	if err := verifyImageChecksum(context.Background(), url, "", path); err != nil {
		t.Errorf("expected nil error for GitHub Release with missing sidecar, got %v", err)
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
	if err := verifyImageChecksum(context.Background(), "http://example.invalid/image", want, path); err != nil {
		t.Errorf("verifyImageChecksum with matching pinned SHA-512 error = %v", err)
	}

	// Mismatched embedded SHA-512: fatal, and never touches the sidecar.
	wrong := strings.Repeat("a", 128)
	err := verifyImageChecksum(context.Background(), "http://example.invalid/image", wrong, path)
	if err == nil {
		t.Fatal("expected SHA-512 mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "SHA-512 mismatch") {
		t.Errorf("expected SHA-512 mismatch error, got %v", err)
	}
}
