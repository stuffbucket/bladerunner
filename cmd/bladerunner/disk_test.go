package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/disk"
)

// withDiskNewFlags installs a clean `br disk new` flag set (they are package
// globals bound by cobra) and an isolated disks directory, so a test writes a
// manifest under its own temp dir and never near the user's.
func withDiskNewFlags(t *testing.T) {
	t.Helper()
	saved := diskNewFlags
	diskNewFlags.from = ""
	diskNewFlags.gui = false
	diskNewFlags.force = false
	diskNewFlags.arch = ""
	diskNewFlags.size = 0
	t.Cleanup(func() { diskNewFlags = saved })
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

// loadScaffolded reads back the manifest `br disk new` just wrote.
func loadScaffolded(t *testing.T, name string) *disk.Manifest {
	t.Helper()
	path := filepath.Join(disk.DefaultDiskDir(), name+disk.ManifestExt)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read scaffolded disk %s: %v", path, err)
	}
	var m disk.Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode scaffolded disk %s: %v", path, err)
	}
	return &m
}

// archNames lists the architectures a manifest carries an image for.
func archNames(m *disk.Manifest) []string {
	names := make([]string, 0, len(m.Image.Arches))
	for a := range m.Image.Arches {
		names = append(names, a)
	}
	return names
}

// `br disk new --arch` was declared and never read: runDiskNew always wrote
// both an arm64 and an amd64 entry, so the flag was a promise the scaffold did
// not keep. It now selects the architecture written.
func TestDiskNewHonoursArch(t *testing.T) {
	withDiskNewFlags(t)
	diskNewFlags.arch = "amd64"

	if err := runDiskNew(nil, []string{"mine"}); err != nil {
		t.Fatalf("disk new --arch amd64: %v", err)
	}
	m := loadScaffolded(t, "mine")
	got := archNames(m)
	if len(got) != 1 || m.Image.Arches["amd64"].URL == "" {
		t.Fatalf("--arch amd64 scaffolded %v, want amd64 alone", got)
	}
	if !strings.Contains(m.Image.Arches["amd64"].URL, "amd64") {
		t.Errorf("amd64 entry points at %q", m.Image.Arches["amd64"].URL)
	}
}

// With no --arch the scaffold stays portable: every architecture the pinned
// image publishes, which is what it always wrote.
func TestDiskNewScaffoldsEveryArchByDefault(t *testing.T) {
	withDiskNewFlags(t)

	if err := runDiskNew(nil, []string{"mine"}); err != nil {
		t.Fatalf("disk new: %v", err)
	}
	m := loadScaffolded(t, "mine")
	for _, want := range []string{"arm64", "amd64"} {
		if m.Image.Arches[want].URL == "" {
			t.Errorf("default scaffold is missing %s (got %v)", want, archNames(m))
		}
	}
}

// An architecture the base image is not published for is refused, rather than
// written as an empty URL.
func TestDiskNewRejectsAnUnknownArch(t *testing.T) {
	withDiskNewFlags(t)
	diskNewFlags.arch = "riscv64"

	err := runDiskNew(nil, []string{"mine"})
	if err == nil {
		t.Fatal("an unsupported --arch was accepted")
	}
	if !strings.Contains(err.Error(), "riscv64") {
		t.Errorf("error %q does not name the architecture", err)
	}
}

// --size was read only in the scaffold branch, so `br disk new x --from y
// --size 99` silently kept the forked disk's size. It now applies to a fork
// too.
func TestDiskNewAppliesSizeToAFork(t *testing.T) {
	withDiskNewFlags(t)
	diskNewFlags.from = "incus"
	diskNewFlags.size = 99

	if err := runDiskNew(nil, []string{"forked"}); err != nil {
		t.Fatalf("disk new --from incus --size 99: %v", err)
	}
	if got := loadScaffolded(t, "forked").VM.DiskSizeGiB; got != 99 {
		t.Fatalf("--size 99 on a fork produced %d GiB", got)
	}
}

// Without --size a fork keeps the size it was forked from, which is what
// --from promises ("fork an existing catalog disk's image and sizing").
func TestDiskNewForkKeepsItsSizeWithoutTheFlag(t *testing.T) {
	withDiskNewFlags(t)
	diskNewFlags.from = "incus"

	if err := runDiskNew(nil, []string{"forked"}); err != nil {
		t.Fatalf("disk new --from incus: %v", err)
	}
	if got := loadScaffolded(t, "forked").VM.DiskSizeGiB; got != 64 {
		t.Fatalf("a fork's size = %d GiB, want the forked disk's 64", got)
	}
}

// A plain scaffold with no --size still gets the global default.
func TestDiskNewDefaultsTheSize(t *testing.T) {
	withDiskNewFlags(t)

	if err := runDiskNew(nil, []string{"mine"}); err != nil {
		t.Fatalf("disk new: %v", err)
	}
	if got := loadScaffolded(t, "mine").VM.DiskSizeGiB; got != config.DefaultDiskSizeGiB {
		t.Fatalf("default size = %d GiB, want %d", got, config.DefaultDiskSizeGiB)
	}
}

// --arch narrows a fork's per-arch images too, so the flag means one thing on
// both paths.
func TestDiskNewArchNarrowsAFork(t *testing.T) {
	withDiskNewFlags(t)
	diskNewFlags.from = "debian-trixie-gui"
	diskNewFlags.arch = "arm64"

	if err := runDiskNew(nil, []string{"forked"}); err != nil {
		t.Fatalf("disk new --from debian-trixie-gui --arch arm64: %v", err)
	}
	m := loadScaffolded(t, "forked")
	if got := archNames(m); len(got) != 1 || m.Image.Arches["arm64"].URL == "" {
		t.Fatalf("--arch arm64 on a fork kept %v, want arm64 alone", got)
	}
}

// A fork of a disk that carries no per-arch images cannot honour --arch, so it
// says so rather than dropping the flag.
func TestDiskNewArchRefusesAForkWithoutPerArchImages(t *testing.T) {
	withDiskNewFlags(t)
	diskNewFlags.from = "incus" // hosted image, no image.arches
	diskNewFlags.arch = "arm64"

	err := runDiskNew(nil, []string{"forked"})
	if err == nil {
		t.Fatal("--arch was silently dropped on a fork with no per-arch images")
	}
	if !strings.Contains(err.Error(), "incus") {
		t.Errorf("error %q does not name the forked disk", err)
	}
}
