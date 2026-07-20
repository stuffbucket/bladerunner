package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type tarEntry struct {
	name string
	body string
}

// buildRawTarball writes the given entries verbatim into a gzip tarball.
func buildRawTarball(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// buildAppTarball builds a gzip tarball rooted at "Bladerunner.app" with the
// given files (paths relative to the .app root).
func buildAppTarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	const appName = "Bladerunner.app"
	entries := make([]tarEntry, 0, len(files))
	for rel, body := range files {
		entries = append(entries, tarEntry{name: appName + "/" + rel, body: body})
	}
	return buildRawTarball(t, entries)
}

// TestApply_EndToEnd exercises the full download → verify → extract → swap flow
// against an httptest server, using a throwaway keypair. It installs into a
// simulated /Applications by pointing ExecPath at a temp .app bundle.
func TestApply_EndToEnd(t *testing.T) {
	kp := newTestKeypair(t)

	// Simulate an installed bundle whose binary is the "running" process.
	root := t.TempDir()
	bundle := filepath.Join(root, "Bladerunner.app")
	if err := os.MkdirAll(filepath.Join(bundle, "Contents", "MacOS"), 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(bundle, "Contents", "MacOS", "br")
	if err := os.WriteFile(exe, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Build the new signed artifact.
	tarball := buildAppTarball(t, map[string]string{
		"Contents/MacOS/br":   "new-binary",
		"Contents/Info.plist": "<plist/>",
	})
	sig := kp.sign(tarball, "timestamp:1\tfile:Bladerunner.app.tar.gz")

	mux := http.NewServeMux()
	mux.HandleFunc("/artifact.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarball)
	})
	var srv *httptest.Server
	mux.HandleFunc("/latest.json", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(Manifest{
			Version:   "0.9.9",
			URL:       srv.URL + "/artifact.tar.gz",
			Signature: sig,
		})
	})
	srv = httptest.NewTLSServer(mux)
	defer srv.Close()

	opts := Options{
		CurrentVersion: "0.4.7",
		ManifestURL:    srv.URL + "/latest.json",
		PublicKey:      kp.pubKeyB64(),
		HTTPClient:     srv.Client(),
		ExecPath:       exe,
		Relaunch:       false, // never spawn a real process in tests
	}

	res, err := Apply(context.Background(), opts)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.ToVersion != "0.9.9" || res.FromVersion != "0.4.7" {
		t.Fatalf("versions = %s -> %s", res.FromVersion, res.ToVersion)
	}

	// The installed binary must now be the new one.
	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("read installed binary: %v", err)
	}
	if string(got) != "new-binary" {
		t.Fatalf("binary = %q want new-binary", got)
	}
}

// TestApply_RejectsTamperedArtifact proves the fail-closed guarantee end-to-end:
// a tarball that does not match its signature is never installed.
func TestApply_RejectsTamperedArtifact(t *testing.T) {
	kp := newTestKeypair(t)

	root := t.TempDir()
	bundle := filepath.Join(root, "Bladerunner.app")
	exe := filepath.Join(bundle, "Contents", "MacOS", "br")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	good := buildAppTarball(t, map[string]string{"Contents/MacOS/br": "legit"})
	sig := kp.sign(good, "timestamp:1\tfile:x")
	// Serve a DIFFERENT tarball than the one that was signed.
	evil := buildAppTarball(t, map[string]string{"Contents/MacOS/br": "malware"})

	mux := http.NewServeMux()
	mux.HandleFunc("/artifact.tar.gz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(evil) })
	var srv *httptest.Server
	mux.HandleFunc("/latest.json", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(Manifest{Version: "0.9.9", URL: srv.URL + "/artifact.tar.gz", Signature: sig})
	})
	srv = httptest.NewTLSServer(mux)
	defer srv.Close()

	_, err := Apply(context.Background(), Options{
		CurrentVersion: "0.4.7",
		ManifestURL:    srv.URL + "/latest.json",
		PublicKey:      kp.pubKeyB64(),
		HTTPClient:     srv.Client(),
		ExecPath:       exe,
	})
	if err == nil {
		t.Fatal("expected Apply to REFUSE a tampered artifact")
	}
	if !strings.Contains(err.Error(), "unverified artifact") {
		t.Fatalf("expected unverified-artifact error, got: %v", err)
	}
	// The old binary must be untouched.
	got, _ := os.ReadFile(exe)
	if string(got) != "old-binary" {
		t.Fatalf("binary was modified despite verify failure: %q", got)
	}
}

// TestApply_HomebrewRefused proves a Homebrew-managed install is refused before
// any network work — no server is even started.
func TestApply_HomebrewRefused(t *testing.T) {
	_, err := Apply(context.Background(), Options{
		CurrentVersion: "0.4.7",
		ExecPath:       "/opt/homebrew/Cellar/bladerunner/0.4.7/bin/br",
		ManifestURL:    "https://127.0.0.1:1/should-never-be-hit",
	})
	if !errors.Is(err, ErrHomebrewManaged) {
		t.Fatalf("expected ErrHomebrewManaged, got %v", err)
	}
}

func TestApply_NoUpdateWhenNotNewer(t *testing.T) {
	kp := newTestKeypair(t)
	root := t.TempDir()
	exe := filepath.Join(root, "Bladerunner.app", "Contents", "MacOS", "br")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("cur"), 0o755); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(Manifest{Version: "0.4.0", URL: "https://x/y.tar.gz", Signature: "s"})
	}))
	defer srv.Close()

	res, err := Apply(context.Background(), Options{
		CurrentVersion: "0.4.7",
		ManifestURL:    srv.URL,
		PublicKey:      kp.pubKeyB64(),
		HTTPClient:     srv.Client(),
		ExecPath:       exe,
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Relaunched {
		t.Fatal("relaunched on a no-op update")
	}
	// The key guarantee: the installed binary is untouched when the manifest is
	// not newer than the running version.
	if string(mustRead(t, exe)) != "cur" {
		t.Fatal("binary changed on a no-op update")
	}
}

func TestCheck(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(Manifest{Version: "1.0.0", URL: "https://x/y.tar.gz", Signature: "s", Notes: "big release"})
	}))
	defer srv.Close()

	res, err := Check(context.Background(), Options{
		CurrentVersion: "0.4.7",
		ManifestURL:    srv.URL,
		HTTPClient:     srv.Client(),
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !res.UpdateAvailable {
		t.Fatal("expected update available")
	}
	if res.LatestVersion != "1.0.0" || res.Notes != "big release" {
		t.Fatalf("unexpected result %+v", res)
	}
}

func TestCheck_UpToDate(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(Manifest{Version: "0.4.7", URL: "https://x/y.tar.gz", Signature: "s"})
	}))
	defer srv.Close()

	res, err := Check(context.Background(), Options{
		CurrentVersion: "0.4.7",
		ManifestURL:    srv.URL,
		HTTPClient:     srv.Client(),
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.UpdateAvailable {
		t.Fatal("expected up-to-date")
	}
}

func TestSHA256Hex(t *testing.T) {
	// Known vector for the empty string.
	if got := sha256Hex(nil); got != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatalf("sha256Hex(nil) = %s", got)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	return b
}

// TestApply_EmptyPublicKeyFailsClosed guards against a build that forgot to
// wire the production key: with no key, even a well-formed signed artifact is
// refused.
func TestApply_EmptyPublicKeyFailsClosed(t *testing.T) {
	kp := newTestKeypair(t)
	root := t.TempDir()
	exe := filepath.Join(root, "Bladerunner.app", "Contents", "MacOS", "br")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	tarball := buildAppTarball(t, map[string]string{"Contents/MacOS/br": "new"})
	sig := kp.sign(tarball, "timestamp:1\tfile:x")

	mux := http.NewServeMux()
	mux.HandleFunc("/artifact.tar.gz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(tarball) })
	var srv *httptest.Server
	mux.HandleFunc("/latest.json", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(Manifest{Version: "9.9.9", URL: srv.URL + "/artifact.tar.gz", Signature: sig})
	})
	srv = httptest.NewTLSServer(mux)
	defer srv.Close()

	// PublicKey empty -> falls back to the (empty placeholder) productionPublicKey.
	_, err := Apply(context.Background(), Options{
		CurrentVersion: "0.4.7",
		ManifestURL:    srv.URL + "/latest.json",
		HTTPClient:     srv.Client(),
		ExecPath:       exe,
	})
	if err == nil {
		t.Fatal("expected refusal when no public key is configured")
	}
	if string(mustRead(t, exe)) != "old" {
		t.Fatal("binary changed despite missing public key")
	}
}
