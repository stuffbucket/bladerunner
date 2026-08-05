//go:build linux

package imagebuild

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A real bake, end to end: fetch the reviewed base, grow it, apply the recipe
// through the nbd + chroot mechanic, compress, and publish.
//
// Nothing else exercises the pipeline as a whole. Every other test in this
// package injects the operations so the ORDER can be asserted without root, a
// device or a network — which is the right trade for the ninety-nine percent of
// changes that do not touch the mechanic, and no substitute for having run it
// once. Until this passed, the Go pipeline had never produced an image.
//
// Behind -short per AGENTS.md 6.4: it wants root, an nbd device, a network, and
// several minutes. Run it with:
//
//	docker run --rm --privileged -v "$PWD":/src:ro -w /src <image> \
//	    go test -run TestBakeProducesAnImage -v ./internal/imagebuild/
func TestBakeProducesAnImage(t *testing.T) {
	if testing.Short() {
		t.Skip("a real bake needs root, an nbd device and a network")
	}
	requireBakeHost(t)

	work := t.TempDir()
	out := filepath.Join(t.TempDir(), "guest.qcow2")

	release, err := BaseRelease(hostDebianArch(t))
	if err != nil {
		t.Fatalf("BaseRelease: %v", err)
	}
	plan, err := NewBakePlan(release, DefaultRecipe(testVersion), work, out, defaultBakeSizeGiB)
	if err != nil {
		t.Fatalf("NewBakePlan: %v", err)
	}

	logf := func(line string) { t.Log(line) }
	mechanic, err := HostMechanic(work, logf)
	if err != nil {
		t.Fatalf("HostMechanic: %v", err)
	}
	if _, err := Bake(t.Context(), plan, NewBakeDeps(mechanic, logf)); err != nil {
		t.Fatalf("Bake: %v", err)
	}

	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("the bake reported success but wrote no image: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("the published image is empty")
	}
	// The partial must be gone: publish is a rename, so a leftover means the
	// image took its final name by some other route.
	if _, err := os.Stat(plan.PartialPath); err == nil {
		t.Errorf("the partial %s survived; publish did not rename it", plan.PartialPath)
	}
	t.Logf("baked %s (%d bytes)", out, info.Size())
}

// defaultBakeSizeGiB is the working size a bake grows the base image to. It
// matches what the shell build and `br disk bake` use.
const defaultBakeSizeGiB = 8

// requireBakeHost skips unless this machine can actually run the native
// mechanic, so the test reports "not applicable" rather than failing on a
// developer laptop.
func requireBakeHost(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("the native mechanic needs root to mount and chroot")
	}
	if _, err := os.Stat(defaultNBDDevice); err != nil {
		t.Skipf("%s is absent; the native mechanic has nothing to attach to", defaultNBDDevice)
	}
	for _, tool := range []string{"qemu-img", "qemu-nbd"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
}

// hostDebianArch names this machine in Debian's terms, which is what the
// pinned releases are keyed by.
func hostDebianArch(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("dpkg", "--print-architecture").Output()
	if err != nil {
		t.Skipf("dpkg is not available to name this architecture: %v", err)
	}
	return string(trimLine(out))
}

// trimLine drops a trailing newline without pulling in strings for one call.
func trimLine(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
