package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/disk"
)

// sentinelBody is written to the bake output path before the command runs. Its
// survival is the evidence that nothing built anything.
const sentinelBody = "a file the user already had at this path"

// writeUserDisk puts a manifest in the isolated user disk directory and returns
// its path.
func writeUserDisk(t *testing.T, name string, m disk.Manifest) string {
	t.Helper()
	dir := disk.DefaultDiskDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create disk dir: %v", err)
	}
	path := filepath.Join(dir, name+disk.ManifestExt)
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}

// withBakeFlags installs a clean `br disk bake` flag set and an isolated disks
// directory, since the flags are package globals bound by cobra.
func withBakeFlags(t *testing.T, out string) {
	t.Helper()
	saved := diskBakeFlags
	t.Cleanup(func() { diskBakeFlags = saved })
	diskBakeFlags.arch = "arm64"
	diskBakeFlags.output = out
	diskBakeFlags.size = 8
	diskBakeFlags.release = "trixie"
	diskBakeFlags.timeoutMin = 1
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

// A disk that bake can never record a result into must be refused BEFORE
// anything is built.
//
// The manifest shape is known the moment it is loaded, but the check used to
// sit after the build subprocess had already downloaded a base image,
// customized it, compressed it and renamed it into --output. So the user paid
// the full build for a guaranteed refusal, and an existing file at that path
// was replaced by an image the command then declined to reference.
//
// The output file is the assertion that matters. An error message can be made
// to appear early while the damage still happens; a byte-identical sentinel
// cannot.
func TestBakeRefusesANonPerArchDiskBeforeBuilding(t *testing.T) {
	out := filepath.Join(t.TempDir(), "sentinel.qcow2")
	withBakeFlags(t, out)
	if err := os.WriteFile(out, []byte(sentinelBody), 0o644); err != nil {
		t.Fatalf("seed sentinel: %v", err)
	}

	// A hosted disk: legitimate, bootable, and not something bake can stamp.
	writeUserDisk(t, "hosted-one", disk.Manifest{
		Name:  "hosted-one",
		Image: disk.ImageSpec{Hosted: true},
		Boot:  disk.BootSpec{Mode: disk.BootModeHeadless},
	})

	err := runDiskBake(&cobra.Command{}, []string{"hosted-one"})

	if err == nil {
		t.Fatal("bake accepted a hosted disk it cannot record a result into")
	}
	if !strings.Contains(err.Error(), "per-arch") {
		t.Errorf("error = %q, want it to explain the disk is not per-arch;\n"+
			"a build-tool or script error here means the refusal came after the build", err)
	}

	got, readErr := os.ReadFile(out)
	if readErr != nil {
		t.Fatalf("the output path is gone; something built over it: %v", readErr)
	}
	if string(got) != sentinelBody {
		t.Errorf("the file at --output was replaced before the disk was refused;\n got %q\nwant %q",
			string(got), sentinelBody)
	}
}

// The same refusal must not fire for a disk bake CAN record into, or the
// preflight would simply block the feature.
func TestBakeAcceptsAPerArchDiskAtPreflight(t *testing.T) {
	withBakeFlags(t, filepath.Join(t.TempDir(), "out.qcow2"))
	m := disk.Manifest{
		Name: "per-arch-one",
		Image: disk.ImageSpec{Arches: map[string]disk.ArchImage{
			"arm64": {URL: "https://example.invalid/arm64.qcow2"},
		}},
	}

	if err := bakePreflight(&m, "per-arch-one", "arm64"); err != nil {
		t.Errorf("preflight refused a per-arch disk: %v", err)
	}
}

// An architecture the manifest has no slot for is the other shape bake cannot
// record, and it is knowable just as early.
func TestBakePreflightRejectsAnUnknownArch(t *testing.T) {
	m := disk.Manifest{
		Name: "per-arch-one",
		Image: disk.ImageSpec{Arches: map[string]disk.ArchImage{
			"arm64": {URL: "https://example.invalid/arm64.qcow2"},
		}},
	}

	if err := bakePreflight(&m, "per-arch-one", "riscv64"); err == nil {
		t.Error("preflight accepted an architecture the manifest has no entry for")
	}
}
