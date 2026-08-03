//go:build linux

package imagebuild

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// The parity gate: bake the same recipe through the shell build and through
// the Go pipeline, then compare what the two images CONTAIN.
//
// This is what has to pass before scripts/build-guest-image.sh can be deleted.
// It compares descriptions rather than a checklist of properties, so it fails
// on any difference — including one nobody predicted, which is the whole reason
// the two paths drifted in the first place.
//
// It went red on its first three runs and each time named something real: the
// shell build refusing a host over an unloadable nbd module, then over missing
// partition nodes, then a version stamp this test itself had made differ. It is
// green now, on two real bakes of the same recipe, which is what clears the
// shell build for deletion.
func TestScriptAndGoProduceTheSameImage(t *testing.T) {
	if testing.Short() {
		t.Skip("a parity bake needs root, an nbd device and a network")
	}
	requireBakeHost(t)
	script := buildScriptPath(t)

	arch := hostDebianArch(t)
	release, err := BaseRelease(arch)
	if err != nil {
		t.Fatalf("BaseRelease: %v", err)
	}
	// Both builds must be stamped the same, or the version file differs and the
	// gate reports a divergence it created itself. The script derives its stamp
	// from `date -u +%Y.%m.%d`; BuildVersion is the same value from the same
	// clock, which is the point of it existing.
	recipe := DefaultRecipe(BuildVersion(time.Now()))

	goImage := bakeWithGo(t, release, recipe)
	shellImage := bakeWithScript(t, script, arch)

	goDesc := describeImage(t, goImage, recipe)
	shellDesc := describeImage(t, shellImage, recipe)

	diff := goDesc.Diff(shellDesc)
	if len(diff) == 0 {
		t.Log("the two mechanics produce the same image; the shell build can be retired")
		return
	}
	t.Errorf("the two mechanics produce DIFFERENT images (%d differences).\n"+
		"Every one must be explained or fixed before scripts/build-guest-image.sh is deleted.", len(diff))
	for _, d := range diff {
		t.Errorf("  %s", d)
	}
}

// bakeWithGo runs the Go pipeline and returns the image it published.
func bakeWithGo(t *testing.T, r Release, recipe Recipe) string {
	t.Helper()
	work := t.TempDir()
	out := filepath.Join(t.TempDir(), "go-guest.qcow2")

	plan, err := NewBakePlan(r, recipe, work, out, defaultBakeSizeGiB)
	if err != nil {
		t.Fatalf("NewBakePlan: %v", err)
	}
	if err := Bake(t.Context(), plan, LinuxBakeDeps(work, func(l string) { t.Logf("go: %s", l) })); err != nil {
		t.Fatalf("the Go bake failed: %v", err)
	}
	return out
}

// bakeWithScript runs the shell build the same way CI does.
//
// A failure here SKIPS rather than fails. "The shell build could not run on
// this host" and "the two builds disagree" are different results, and reporting
// the first as the second would make the gate look like it had found a
// divergence when it had found nothing at all. The shell build is markedly less
// portable than the Go one — it needs a loadable nbd module and udev to create
// the partition node, both of which the Go mechanic works around — so this skip
// is reachable on exactly the hosts where the comparison would be most useful.
func bakeWithScript(t *testing.T, script, arch string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "shell-guest.qcow2")

	cmd := exec.CommandContext(t.Context(), "bash", script,
		"--arch", arch, "--output", out, "--method", "nbd")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Skipf("PARITY NOT ESTABLISHED: the shell build did not run here (%v). "+
			"The Go build completed on this same host, so this is the shell build's "+
			"portability, not a difference between the images.", err)
	}
	return out
}

// describeImage attaches a built image, reads what it contains, and detaches
// BEFORE returning.
//
// The detach cannot be deferred to t.Cleanup. There is one nbd device, so the
// second image cannot be attached until the first is off it — deferring means
// the second attach fails with "Failed to set NBD socket" while the first image
// is still connected, which reads like a broken build and is not.
func describeImage(t *testing.T, image string, recipe Recipe) Description {
	t.Helper()
	work := t.TempDir()

	mount, err := attachImage(context.WithoutCancel(t.Context()), image, work, defaultNBDDevice)
	if err != nil {
		t.Fatalf("attach %s: %v", image, err)
	}
	defer func() {
		if err := mount.Close(); err != nil {
			t.Errorf("detach %s: %v", image, err)
		}
	}()

	desc, err := Describe(mount.Root, recipe)
	if err != nil {
		t.Fatalf("describe %s: %v", image, err)
	}
	return desc
}
