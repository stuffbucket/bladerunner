package provision_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/provision"
)

// seedFilePerm is the mode this test stages a prior generation with. It matches
// what WriteSeedFiles publishes, so the comparison is about content only.
const seedFilePerm os.FileMode = 0o644

// TestWriteSeedFilesReplacesRatherThanTruncates holds the atomicity half of
// #212.
//
// A reader that opened the seed before the rewrite is the observable form of
// "never a partial file": os.WriteFile opens O_TRUNC and writes in place, so
// that reader watches the previous generation disappear under it and can read a
// truncated or half-written document. util.WriteFileAtomic renames a completed
// file over the old one instead, so the open descriptor keeps the whole
// previous generation and everyone who opens afterwards gets the whole new one.
func TestWriteSeedFilesReplacesRatherThanTruncates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := &config.Config{CloudInitDir: dir}

	const (
		oldUserData = "#cloud-config\nhostname: previous-generation\n"
		oldMetaData = "instance-id: bladerunner-previous\n"
		newUserData = "#cloud-config\nhostname: next-generation\n"
		newMetaData = "instance-id: bladerunner-next\n"
	)
	userPath := filepath.Join(dir, "user-data")
	metaPath := filepath.Join(dir, "meta-data")
	writeFile(t, userPath, oldUserData)
	writeFile(t, metaPath, oldMetaData)

	// Opened BEFORE the rewrite, exactly as a guest datasource or a concurrent
	// reader would have it.
	userReader := openFile(t, userPath)
	metaReader := openFile(t, metaPath)

	if err := provision.WriteSeedFiles(cfg, newUserData, newMetaData); err != nil {
		t.Fatalf("WriteSeedFiles: %v", err)
	}

	for _, tc := range []struct {
		name   string
		reader *os.File
		want   string
	}{
		{"user-data", userReader, oldUserData},
		{"meta-data", metaReader, oldMetaData},
	} {
		got, err := io.ReadAll(tc.reader)
		if err != nil {
			t.Fatalf("read the pre-opened %s: %v", tc.name, err)
		}
		if string(got) != tc.want {
			t.Errorf("a reader holding %s across the rewrite saw %q, want the whole previous generation %q",
				tc.name, string(got), tc.want)
		}
	}

	// The published generation is the new one, and nothing else is left behind.
	if got := readFile(t, userPath); got != newUserData {
		t.Errorf("published user-data = %q, want %q", got, newUserData)
	}
	if got := readFile(t, metaPath); got != newMetaData {
		t.Errorf("published meta-data = %q, want %q", got, newMetaData)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the seed dir: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("seed dir holds %d entries, want exactly user-data and meta-data (staging residue left behind)", len(entries))
	}
}

// TestBuildCloudInitISOKeepsThePriorISOOnFailure holds the transactional half
// of #212.
//
// The ISO used to be removed before hdiutil was asked for a replacement, so any
// failure — a canceled context, a missing tool, a full disk — left the VM with
// no seed volume at all where a moment earlier it had a working one. A build
// that fails must leave the previous generation exactly as it was, and must not
// leave staged output behind either.
//
// The context is canceled before the call, which fails the build identically on
// a host with hdiutil and on one without, so this holds on every platform.
func TestBuildCloudInitISOKeepsThePriorISOOnFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedDir := filepath.Join(dir, "cloud-init")
	if err := os.MkdirAll(seedDir, 0o755); err != nil {
		t.Fatalf("create the seed dir: %v", err)
	}
	writeFile(t, filepath.Join(seedDir, "user-data"), "#cloud-config\n")
	writeFile(t, filepath.Join(seedDir, "meta-data"), "instance-id: x\n")

	isoPath := filepath.Join(dir, "cidata.iso")
	const priorISO = "the previous complete seed volume"
	writeFile(t, isoPath, priorISO)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cfg := &config.Config{CloudInitDir: seedDir, CloudInitISO: isoPath}
	if err := provision.BuildCloudInitISO(ctx, cfg); err == nil {
		t.Fatal("BuildCloudInitISO reported success although the build was canceled")
	}

	got, err := os.ReadFile(isoPath)
	if err != nil {
		t.Fatalf("the prior ISO was destroyed by a failed build: %v", err)
	}
	if string(got) != priorISO {
		t.Errorf("prior ISO = %q, want it untouched at %q", string(got), priorISO)
	}

	// Staged output must not survive the failure either.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the ISO dir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "cidata.iso" && e.Name() != "cloud-init" {
			t.Errorf("failed build left %q behind in the ISO directory", e.Name())
		}
	}
}

// TestBuildCloudInitISOPublishesAWorkingVolume holds the success path across
// the staging rework: hdiutil now writes into a private directory and the
// result is renamed into place, so the claim that a seed volume still arrives
// at cfg.CloudInitISO is about what hdiutil actually does, not about the code
// that calls it.
//
// It needs the real tool, so it runs on macOS only and is skipped in short mode.
func TestBuildCloudInitISOPublishesAWorkingVolume(t *testing.T) {
	if testing.Short() {
		t.Skip("runs hdiutil")
	}
	if runtime.GOOS != "darwin" {
		t.Skipf("hdiutil is macOS only; this host is %s", runtime.GOOS)
	}
	t.Parallel()

	dir := t.TempDir()
	seedDir := filepath.Join(dir, "cloud-init")
	if err := os.MkdirAll(seedDir, 0o755); err != nil {
		t.Fatalf("create the seed dir: %v", err)
	}
	writeFile(t, filepath.Join(seedDir, "user-data"), "#cloud-config\nhostname: baked\n")
	writeFile(t, filepath.Join(seedDir, "meta-data"), "instance-id: bladerunner-baked\n")

	isoPath := filepath.Join(dir, "cidata.iso")
	writeFile(t, isoPath, "the previous complete seed volume")

	cfg := &config.Config{CloudInitDir: seedDir, CloudInitISO: isoPath}
	if err := provision.BuildCloudInitISO(context.Background(), cfg); err != nil {
		t.Fatalf("BuildCloudInitISO: %v", err)
	}

	// The published file is where the VM configuration attaches it, is a real
	// ISO 9660 volume, and carries the label the NoCloud datasource looks for.
	body, err := os.ReadFile(isoPath)
	if err != nil {
		t.Fatalf("read the published ISO: %v", err)
	}
	const pvdOffset = 16 * 2048
	if len(body) < pvdOffset+6 {
		t.Fatalf("published ISO is %d bytes, too small to hold a volume descriptor", len(body))
	}
	if got := string(body[pvdOffset+1 : pvdOffset+6]); got != "CD001" {
		t.Errorf("published file has no ISO 9660 primary volume descriptor (found %q)", got)
	}
	if !strings.Contains(string(body), "CIDATA") {
		t.Error("published ISO does not carry the cidata volume label the guest datasource searches for")
	}

	// Staging is transient: nothing beside the destination survives the build.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the ISO dir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "cidata.iso" && e.Name() != "cloud-init" {
			t.Errorf("a successful build left %q behind in the ISO directory", e.Name())
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), seedFilePerm); err != nil {
		t.Fatalf("stage %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func openFile(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}
