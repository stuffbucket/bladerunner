package imagebuild

import (
	"bytes"
	"crypto/sha512"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeMirror serves one image and a manifest describing it, so acquisition can
// be exercised without reaching the real Debian mirror.
type fakeMirror struct {
	server   *httptest.Server
	image    []byte
	manifest string
	// hits counts image requests, to prove a cached image is not refetched.
	hits int
}

func newFakeMirror(t *testing.T, body []byte, manifestDigest string) *fakeMirror {
	t.Helper()
	m := &fakeMirror{image: body}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "SHA512SUMS"):
			_, _ = w.Write([]byte(m.manifest))
		case strings.HasSuffix(r.URL.Path, ".qcow2"):
			m.hits++
			_, _ = w.Write(m.image)
		default:
			http.NotFound(w, r)
		}
	})
	m.server = httptest.NewServer(mux)
	t.Cleanup(m.server.Close)

	name := filepath.Base(mustRelease(t, "arm64").FileName())
	m.manifest = manifestDigest + "  " + name + "\n"
	return m
}

func mustRelease(t *testing.T, arch string) Release {
	t.Helper()
	r, err := BaseRelease(arch)
	if err != nil {
		t.Fatalf("BaseRelease(%q): %v", arch, err)
	}
	return r
}

func sha512Hex(b []byte) string {
	sum := sha512.Sum512(b)
	return hex.EncodeToString(sum[:])
}

// pinFor overrides the pinned digest for the duration of a test, so a test can
// pin a synthetic image rather than a 350MB download.
func pinFor(t *testing.T, name, digest string) {
	t.Helper()
	previous := basePins
	basePins = map[string]string{name: digest}
	t.Cleanup(func() { basePins = previous })
}

// The repository must pin every architecture it can build for. A stamp bump
// that forgets a digest would otherwise fail at build time on a machine that
// has already started work.
func TestEveryBuildableArchIsPinned(t *testing.T) {
	for _, arch := range []string{"arm64", "amd64"} {
		r := mustRelease(t, arch)
		if _, ok := basePins[r.FileName()]; !ok {
			t.Errorf("no pinned digest for %s; add it from %s", r.FileName(), r.ManifestURL())
		}
	}
}

// The URL must address the immutable dated directory, not "latest". A rebuild
// from an old commit has to get the same bytes.
func TestReleaseURLIsImmutable(t *testing.T) {
	r := mustRelease(t, "arm64")
	if strings.Contains(r.URL(), "/latest/") {
		t.Errorf("release URL %q uses the mutable latest directory", r.URL())
	}
	if !strings.Contains(r.URL(), r.Stamp) {
		t.Errorf("release URL %q does not name the dated release %q", r.URL(), r.Stamp)
	}
	if !strings.HasSuffix(r.URL(), r.FileName()) {
		t.Errorf("release URL %q does not end in the release filename %q", r.URL(), r.FileName())
	}
}

func TestFetchAcceptsAnImageMatchingItsPin(t *testing.T) {
	body := []byte("a pretend qcow2")
	digest := sha512Hex(body)
	r := mustRelease(t, "arm64")
	pinFor(t, r.FileName(), digest)

	mirror := newFakeMirror(t, body, digest)
	dest := filepath.Join(t.TempDir(), "base.qcow2")

	if err := FetchBase(t.Context(), r, dest, mirror.server.URL); err != nil {
		t.Fatalf("FetchBase: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read fetched image: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Error("fetched image differs from what the mirror served")
	}
}

// A single changed byte must fail, and must not leave the bad image at the
// destination where a later step would consume it.
func TestFetchRejectsAnImageThatDoesNotMatchItsPin(t *testing.T) {
	body := []byte("a pretend qcow2")
	r := mustRelease(t, "arm64")
	pinFor(t, r.FileName(), sha512Hex([]byte("what we expected instead")))

	mirror := newFakeMirror(t, body, sha512Hex(body))
	dest := filepath.Join(t.TempDir(), "base.qcow2")

	err := FetchBase(t.Context(), r, dest, mirror.server.URL)
	if err == nil {
		t.Fatal("FetchBase accepted an image that does not match its pin")
	}
	if !strings.Contains(err.Error(), "digest") {
		t.Errorf("error %q does not mention the digest mismatch", err)
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Error("the rejected image was left at the destination")
	}
}

// An architecture with no pin must fail before anything is downloaded, rather
// than fetching hundreds of megabytes and then discovering it cannot be checked.
func TestFetchRefusesAnUnpinnedRelease(t *testing.T) {
	r := mustRelease(t, "arm64")
	pinFor(t, "some-other-image.qcow2", sha512Hex([]byte("x")))

	mirror := newFakeMirror(t, []byte("body"), sha512Hex([]byte("body")))
	err := FetchBase(t.Context(), r, filepath.Join(t.TempDir(), "base.qcow2"), mirror.server.URL)

	if err == nil {
		t.Fatal("FetchBase proceeded with no pinned digest")
	}
	if !strings.Contains(err.Error(), "no pinned") {
		t.Errorf("error %q does not explain the missing pin", err)
	}
	if mirror.hits != 0 {
		t.Errorf("the image was downloaded %d time(s) before the pin was checked", mirror.hits)
	}
}

// An image already present and matching its pin is not downloaded again. A
// guest image is hundreds of megabytes and CI rebuilds are frequent.
func TestFetchReusesAVerifiedLocalImage(t *testing.T) {
	body := []byte("a pretend qcow2")
	digest := sha512Hex(body)
	r := mustRelease(t, "arm64")
	pinFor(t, r.FileName(), digest)

	mirror := newFakeMirror(t, body, digest)
	dest := filepath.Join(t.TempDir(), "base.qcow2")

	for range 3 {
		if err := FetchBase(t.Context(), r, dest, mirror.server.URL); err != nil {
			t.Fatalf("FetchBase: %v", err)
		}
	}
	if mirror.hits != 1 {
		t.Errorf("downloaded %d times, want 1 — a verified local image should be reused", mirror.hits)
	}
}

// A local file that does not match the pin must be replaced, not trusted. This
// is the interrupted-or-tampered cache case.
func TestFetchReplacesAMismatchedLocalImage(t *testing.T) {
	body := []byte("a pretend qcow2")
	digest := sha512Hex(body)
	r := mustRelease(t, "arm64")
	pinFor(t, r.FileName(), digest)

	mirror := newFakeMirror(t, body, digest)
	dest := filepath.Join(t.TempDir(), "base.qcow2")
	if err := os.WriteFile(dest, []byte("truncated or tampered"), 0o600); err != nil {
		t.Fatalf("seed a bad local image: %v", err)
	}

	if err := FetchBase(t.Context(), r, dest, mirror.server.URL); err != nil {
		t.Fatalf("FetchBase: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, body) {
		t.Error("the mismatched local image was not replaced")
	}
}

// Cross-checking the pin against upstream catches a pin that has gone stale,
// which is the failure an immutable URL cannot prevent on its own: Debian can
// respin a dated directory.
func TestVerifyAgainstUpstreamDetectsAStalePin(t *testing.T) {
	body := []byte("a pretend qcow2")
	r := mustRelease(t, "arm64")
	pinFor(t, r.FileName(), sha512Hex(body))

	// The mirror now advertises a different digest than the repository pins.
	mirror := newFakeMirror(t, body, sha512Hex([]byte("upstream respun this")))

	err := VerifyPinAgainstUpstream(t.Context(), r, mirror.server.URL)
	if err == nil {
		t.Fatal("a pin disagreeing with the upstream manifest was accepted")
	}
	if !strings.Contains(err.Error(), "upstream") {
		t.Errorf("error %q does not explain that upstream disagrees", err)
	}
}

func TestVerifyAgainstUpstreamAcceptsAMatchingPin(t *testing.T) {
	body := []byte("a pretend qcow2")
	digest := sha512Hex(body)
	r := mustRelease(t, "arm64")
	pinFor(t, r.FileName(), digest)

	mirror := newFakeMirror(t, body, digest)
	if err := VerifyPinAgainstUpstream(t.Context(), r, mirror.server.URL); err != nil {
		t.Errorf("a pin matching the upstream manifest was rejected: %v", err)
	}
}
