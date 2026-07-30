package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// toolchainLib locates the Go provisioning library relative to this package.
func toolchainLib(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", ".macos-builder", "go-toolchain.sh"))
	if err != nil {
		t.Fatalf("resolve the toolchain library path: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Skipf("toolchain library not present at %s: %v", path, err)
	}
	return path
}

// fakeToolchain builds an archive shaped like Go's, containing a stand-in
// go/bin/go, and returns its path and SHA-256. Using a local archive keeps
// these tests offline and lets them tamper with the bytes.
func fakeToolchain(t *testing.T, dir string) (string, string) {
	t.Helper()
	stage := t.TempDir()
	binDir := filepath.Join(stage, "go", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("stage the fake toolchain: %v", err)
	}
	const body = "#!/bin/sh\necho fake-go\n"
	if err := os.WriteFile(filepath.Join(binDir, "go"), []byte(body), 0o755); err != nil {
		t.Fatalf("write the fake go binary: %v", err)
	}

	archive := filepath.Join(dir, fakeArchiveName)
	tar := exec.Command("tar", "-czf", archive, "-C", stage, "go")
	if out, err := tar.CombinedOutput(); err != nil {
		t.Fatalf("build the fake archive: %v: %s", err, out)
	}

	sum := exec.Command("shasum", "-a", "256", archive)
	out, err := sum.Output()
	if err != nil {
		t.Fatalf("digest the fake archive: %v", err)
	}
	return archive, strings.Fields(string(out))[0]
}

// provisionResult is what one go_provision invocation produced.
type provisionResult struct {
	binDir string
	stderr string
	err    error
}

// provision runs go_provision against a local archive directory. The base URL
// is a file:// URL, so nothing here reaches the network.
func provision(t *testing.T, pinFile, cacheRoot, archiveDir string) provisionResult {
	t.Helper()
	lib := toolchainLib(t)
	script := "set -euo pipefail\n" +
		"source '" + lib + "'\n" +
		"go_provision 1.25.6 '" + cacheRoot + "' '" + pinFile + "' 'file://" + archiveDir + "' darwin-arm64\n"

	cmd := exec.Command("bash", "-c", script)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	return provisionResult{binDir: string(out), stderr: stderr.String(), err: err}
}

// writePin writes a pin file mapping an archive name to a digest.
func writePin(t *testing.T, dir, digest, archive string) string {
	t.Helper()
	path := filepath.Join(dir, "pins.sha256")
	if err := os.WriteFile(path, []byte(digest+"  "+archive+"\n"), 0o600); err != nil {
		t.Fatalf("write the pin file: %v", err)
	}
	return path
}

const fakeArchiveName = "go1.25.6.darwin-arm64.tar.gz"

// A correctly pinned archive installs, and a second run reuses the cached
// archive without fetching again.
func TestToolchainInstallsAndReusesAVerifiedArchive(t *testing.T) {
	archiveDir := t.TempDir()
	_, digest := fakeToolchain(t, archiveDir)
	pin := writePin(t, t.TempDir(), digest, fakeArchiveName)
	cache := t.TempDir()

	first := provision(t, pin, cache, archiveDir)
	if first.err != nil {
		t.Fatalf("first provision failed: %v\n%s", first.err, first.stderr)
	}
	if _, err := os.Stat(filepath.Join(strings.TrimSpace(first.binDir), "go")); err != nil {
		t.Fatalf("no go binary at the reported bin dir %q: %v", first.binDir, err)
	}

	// Removing the source archive proves the second run used the cache: a
	// re-download would now fail.
	if err := os.Remove(filepath.Join(archiveDir, fakeArchiveName)); err != nil {
		t.Fatalf("remove the source archive: %v", err)
	}
	second := provision(t, pin, cache, archiveDir)
	if second.err != nil {
		t.Errorf("second provision did not reuse the cached archive: %v\n%s", second.err, second.stderr)
	}
}

// A single changed byte must fail before anything is extracted or executed.
func TestToolchainRejectsAModifiedArchive(t *testing.T) {
	archiveDir := t.TempDir()
	archive, digest := fakeToolchain(t, archiveDir)
	pin := writePin(t, t.TempDir(), digest, fakeArchiveName)

	body, err := os.ReadFile(archive)
	if err != nil {
		t.Fatalf("read the archive: %v", err)
	}
	body[len(body)/2] ^= 0x01
	//nolint:gosec // G703: archive is inside this test's own temporary directory.
	if err := os.WriteFile(archive, body, 0o600); err != nil {
		t.Fatalf("tamper with the archive: %v", err)
	}

	cache := t.TempDir()
	res := provision(t, pin, cache, archiveDir)
	if res.err == nil {
		t.Fatal("a modified archive was accepted")
	}
	if !strings.Contains(res.stderr, "digest mismatch") {
		t.Errorf("stderr does not report a digest mismatch: %s", res.stderr)
	}
	if _, err := os.Stat(filepath.Join(cache, "1.25.6", "go", "bin", "go")); err == nil {
		t.Error("the toolchain was extracted despite a digest mismatch")
	}
}

// An archive with no pin must fail rather than being downloaded unverified.
func TestToolchainRefusesAnUnpinnedArchive(t *testing.T) {
	archiveDir := t.TempDir()
	fakeToolchain(t, archiveDir)
	// Pin some other file, so the lookup finds nothing for this archive.
	pin := writePin(t, t.TempDir(), strings.Repeat("0", 64), "go9.9.9.darwin-arm64.tar.gz")

	res := provision(t, pin, t.TempDir(), archiveDir)
	if res.err == nil {
		t.Fatal("an unpinned archive was provisioned")
	}
	if !strings.Contains(res.stderr, "no pinned SHA-256") {
		t.Errorf("stderr does not explain the missing pin: %s", res.stderr)
	}
}

// A cached archive that no longer matches its pin must be discarded and
// replaced, not reused. This is the persistent-cache compromise in #236.
func TestToolchainReplacesATamperedCache(t *testing.T) {
	archiveDir := t.TempDir()
	_, digest := fakeToolchain(t, archiveDir)
	pin := writePin(t, t.TempDir(), digest, fakeArchiveName)
	cache := t.TempDir()

	if res := provision(t, pin, cache, archiveDir); res.err != nil {
		t.Fatalf("first provision failed: %v\n%s", res.err, res.stderr)
	}

	cached := filepath.Join(cache, "1.25.6", fakeArchiveName)
	if err := os.WriteFile(cached, []byte("malicious toolchain"), 0o600); err != nil {
		t.Fatalf("tamper with the cached archive: %v", err)
	}

	res := provision(t, pin, cache, archiveDir)
	if res.err != nil {
		t.Fatalf("provision failed instead of replacing the tampered cache: %v\n%s", res.err, res.stderr)
	}
	if !strings.Contains(res.stderr, "failed verification") {
		t.Errorf("stderr does not report the rejected cache: %s", res.stderr)
	}
	body, err := os.ReadFile(cached)
	if err != nil {
		t.Fatalf("read the replaced archive: %v", err)
	}
	if string(body) == "malicious toolchain" {
		t.Error("the tampered archive is still cached")
	}
}

// A tampered `go` binary in the extracted tree must not survive, because the
// tree is rebuilt from the verified archive on every run. A digest recorded
// beside the tree would prove nothing: whatever altered the binary could alter
// the record too.
func TestToolchainRebuildsTheExtractedTree(t *testing.T) {
	archiveDir := t.TempDir()
	_, digest := fakeToolchain(t, archiveDir)
	pin := writePin(t, t.TempDir(), digest, fakeArchiveName)
	cache := t.TempDir()

	first := provision(t, pin, cache, archiveDir)
	if first.err != nil {
		t.Fatalf("first provision failed: %v\n%s", first.err, first.stderr)
	}

	goBinary := filepath.Join(strings.TrimSpace(first.binDir), "go")
	if err := os.WriteFile(goBinary, []byte("#!/bin/sh\necho compromised\n"), 0o755); err != nil {
		t.Fatalf("tamper with the extracted go binary: %v", err)
	}

	if res := provision(t, pin, cache, archiveDir); res.err != nil {
		t.Fatalf("second provision failed: %v\n%s", res.err, res.stderr)
	}
	body, err := os.ReadFile(goBinary)
	if err != nil {
		t.Fatalf("read the go binary: %v", err)
	}
	if strings.Contains(string(body), "compromised") {
		t.Error("a tampered go binary survived a second provision")
	}
}

// The pinned digest for the toolchain the repository actually builds with must
// be present. A go.mod bump that forgets the pin fails the release build on the
// runner; failing here instead says so at review time.
func TestRepositoryPinsItsOwnGoVersion(t *testing.T) {
	pin, err := filepath.Abs(filepath.Join("..", "..", ".macos-builder", "go-toolchain.sha256"))
	if err != nil {
		t.Fatalf("resolve the pin file: %v", err)
	}
	body, err := os.ReadFile(pin)
	if err != nil {
		t.Skipf("pin file not present: %v", err)
	}

	mod, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	var version string
	for _, line := range strings.Split(string(mod), "\n") {
		if after, ok := strings.CutPrefix(line, "go "); ok {
			version = strings.TrimSpace(after)
			break
		}
	}
	if version == "" {
		t.Fatal("no go directive in go.mod")
	}

	want := "go" + version + ".darwin-arm64.tar.gz"
	if !strings.Contains(string(body), want) {
		t.Errorf("%s has no pinned digest for %s; add it from https://go.dev/dl/?mode=json", pin, want)
	}
}
