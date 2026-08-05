package imagebuild

import (
	"slices"
	"strings"
	"testing"
)

// planFor builds a plan for the pinned arm64 release.
func planFor(t *testing.T, workDir, out string, sizeGiB int) BakePlan {
	t.Helper()
	r, err := BaseRelease("arm64")
	if err != nil {
		t.Fatalf("BaseRelease: %v", err)
	}
	p, err := NewBakePlan(r, DefaultRecipe(testVersion), workDir, out, sizeGiB)
	if err != nil {
		t.Fatalf("NewBakePlan: %v", err)
	}
	return p
}

// The order carries dependencies that fail quietly rather than loudly when
// inverted, which is why it is data a test can read rather than the shape of
// some function's control flow.
//
// Resize must precede customize: growing the image afterwards leaves a
// filesystem that does not use the new space, and the guest boots with a disk
// smaller than it asked for. Compress must follow customize, or the work is
// discarded. Publish must be last, because renaming into place is what makes
// the result visible to anything else.
func TestBakePhasesRunInDependencyOrder(t *testing.T) {
	got := planFor(t, t.TempDir(), "/tmp/guest.qcow2", 8).Phases()

	for _, pair := range []struct{ before, after BakePhase }{
		{PhaseFetch, PhaseResize},
		{PhaseResize, PhaseCustomize},
		{PhaseCustomize, PhaseCompress},
		{PhaseCompress, PhasePublish},
	} {
		b, a := slices.Index(got, pair.before), slices.Index(got, pair.after)
		if b < 0 || a < 0 {
			t.Fatalf("phase list %v is missing %s or %s", got, pair.before, pair.after)
		}
		if b > a {
			t.Errorf("%s runs after %s; the order is %v", pair.before, pair.after, got)
		}
	}
}

// The output is written under a partial name and renamed only when complete. A
// bake that dies midway must not leave a truncated image where a later step —
// or a user — would read it as finished.
func TestBakeWritesAPartialUntilItIsComplete(t *testing.T) {
	const out = "/var/tmp/bladerunner-guest-arm64.qcow2"
	p := planFor(t, t.TempDir(), out, 8)

	if p.PartialPath == p.OutputPath {
		t.Fatal("the bake compresses straight to its output; a failure leaves a truncated image in place")
	}
	if !strings.HasPrefix(p.PartialPath, p.OutputPath) {
		t.Errorf("partial %q is not beside the output %q; it should land on the same filesystem so the rename is atomic",
			p.PartialPath, p.OutputPath)
	}
	if got := p.CompressArgs(); got[len(got)-1] != p.PartialPath {
		t.Errorf("compress writes to %q, not the partial %q", got[len(got)-1], p.PartialPath)
	}
}

// The resize target is the size the caller asked for, in the units qemu-img
// reads. A bare number means bytes to qemu-img, which would silently SHRINK the
// image to a few bytes rather than grow it.
func TestBakeResizesInGiB(t *testing.T) {
	p := planFor(t, t.TempDir(), "/tmp/guest.qcow2", 12)

	args := p.ResizeArgs()
	last := args[len(args)-1]
	if last != "12G" {
		t.Errorf("resize target = %q, want %q; a bare number is bytes to qemu-img", last, "12G")
	}
	if !slices.Contains(args, p.BasePath) {
		t.Errorf("resize does not name the working image %q: %v", p.BasePath, args)
	}
}

// Compression is what makes the published image a fraction of the working
// size, and it is only effective because customize zeroed the free space
// first. Losing the flag would quietly publish a full-size image.
func TestBakeCompressesTheOutput(t *testing.T) {
	args := planFor(t, t.TempDir(), "/tmp/guest.qcow2", 8).CompressArgs()

	if !slices.Contains(args, "-c") {
		t.Errorf("compress args carry no -c, so the output is not compressed: %v", args)
	}
	if !slices.Contains(args, qcow2Format) {
		t.Errorf("compress args do not name the qcow2 output format: %v", args)
	}
}

// A plan that cannot produce a usable image must be refused when it is built,
// not when a subprocess fails minutes later.
func TestNewBakePlanRefusesAnUnusablePlan(t *testing.T) {
	r, err := BaseRelease("arm64")
	if err != nil {
		t.Fatalf("BaseRelease: %v", err)
	}
	recipe := DefaultRecipe(testVersion)

	if _, err := NewBakePlan(r, recipe, t.TempDir(), "", 8); err == nil {
		t.Error("a plan with no output path was accepted")
	}
	if _, err := NewBakePlan(r, recipe, t.TempDir(), "/tmp/x.qcow2", 0); err == nil {
		t.Error("a plan with no working size was accepted")
	}
}

// The base image is fetched under its RELEASE name, so a work directory shared
// between two architectures cannot have one bake overwrite the other's base.
func TestBakeNamesTheBaseByItsRelease(t *testing.T) {
	work := t.TempDir()
	arm := planFor(t, work, "/tmp/arm.qcow2", 8)

	amd, err := BaseRelease("amd64")
	if err != nil {
		t.Fatalf("BaseRelease(amd64): %v", err)
	}
	other, err := NewBakePlan(amd, DefaultRecipe(testVersion), work, "/tmp/amd.qcow2", 8)
	if err != nil {
		t.Fatalf("NewBakePlan(amd64): %v", err)
	}

	if arm.BasePath == other.BasePath {
		t.Errorf("both architectures fetch their base to %q; one would overwrite the other", arm.BasePath)
	}
}
