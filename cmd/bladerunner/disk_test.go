package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

// A fork of a disk that carries no per-arch images cannot honor --arch, so it
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

// Bounds for the concurrent-reader hammer below.
const (
	// diskManifestPadBytes makes the payload big enough that a truncating write
	// has a window a concurrent reader can land in.
	diskManifestPadBytes = 1 << 18
	// diskManifestRewrites is how many times the writer republishes the manifest.
	diskManifestRewrites = 200
	// diskManifestReaders is how many goroutines read the manifest concurrently.
	diskManifestReaders = 4
)

// paddedDiskManifest returns a manifest whose serialized form is large enough to
// expose a truncate-then-write race.
func paddedDiskManifest() *disk.Manifest {
	return &disk.Manifest{
		Name:        "hammer",
		Description: strings.Repeat("d", diskManifestPadBytes),
		Image:       disk.ImageSpec{Arches: map[string]disk.ArchImage{"arm64": {URL: "https://x/a.qcow2"}}},
		VM:          disk.VMSpec{DiskSizeGiB: 32},
		Boot:        disk.BootSpec{Mode: disk.BootModeHeadless},
	}
}

// A reader must never see a user disk manifest missing, empty or short while it
// is being rewritten. os.WriteFile opens O_TRUNC, so the destination is briefly
// zero-length; only a temp-file-and-rename publish makes the swap atomic.
func TestWriteManifestNeverExposesAPartialFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hammer"+disk.ManifestExt)
	m := paddedDiskManifest()
	if err := writeManifest(path, m); err != nil {
		t.Fatalf("seed writeManifest: %v", err)
	}
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read seeded manifest: %v", err)
	}
	wantLen := len(full)

	var partials atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range diskManifestReaders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				b, readErr := os.ReadFile(path)
				if readErr != nil || len(b) != wantLen {
					partials.Add(1)
				}
			}
		}()
	}
	for range diskManifestRewrites {
		if err := writeManifest(path, m); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("writeManifest: %v", err)
		}
	}
	close(stop)
	wg.Wait()

	if n := partials.Load(); n != 0 {
		t.Errorf("a concurrent reader saw a partial disk manifest %d times (want %d bytes every time): the publish is not atomic", n, wantLen)
	}
}

// The manifest keeps its published mode across a rewrite. os.CreateTemp makes
// 0600 files, so an atomic publish that forgets to chmod would narrow it and
// a disk a second user could read would become unreadable.
func TestWriteManifestKeepsFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "modes"+disk.ManifestExt)
	m := &disk.Manifest{
		Name:  "modes",
		Image: disk.ImageSpec{Arches: map[string]disk.ArchImage{"arm64": {URL: "https://x/a.qcow2"}}},
		VM:    disk.VMSpec{DiskSizeGiB: 32},
		Boot:  disk.BootSpec{Mode: disk.BootModeHeadless},
	}
	for i := range 2 {
		if err := writeManifest(path, m); err != nil {
			t.Fatalf("writeManifest #%d: %v", i, err)
		}
		st, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat manifest #%d: %v", i, err)
		}
		if perm := st.Mode().Perm(); perm != manifestFilePerm {
			t.Errorf("manifest mode after write #%d = %o, want %o", i, perm, manifestFilePerm)
		}
	}
}

// A completed publish must leave no staging file in the user's disks directory.
func TestWriteManifestLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	name := "tidy" + disk.ManifestExt
	if err := writeManifest(filepath.Join(dir, name), paddedDiskManifest()); err != nil {
		t.Fatalf("writeManifest: %v", err)
	}
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(des) != 1 || des[0].Name() != name {
		t.Errorf("disks directory has %d entries, want only %q", len(des), name)
	}
}
