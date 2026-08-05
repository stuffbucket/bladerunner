package imagebuild

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestBakeReportsTheOptionalStepsItSkipped holds #265.
//
// The optional steps are the web UI ones, and the failure they cover is exactly
// what happened during #263: `apt-get download incus-ui-canonical` returned
// "Unable to locate package", the step was skipped as designed, and the bake
// printed a tick over an image with no web UI in it. The skip was known inside
// the mechanic and got no further.
//
// The mechanic here is the real Apply, so the skip travels the path a bake
// really uses — Apply, mechanic, Bake, caller — rather than a stub asserting
// that a signature exists.
func TestBakeReportsTheOptionalStepsItSkipped(t *testing.T) {
	p := planFor(t, t.TempDir(), filepath.Join(t.TempDir(), "guest.qcow2"), 8)
	root := t.TempDir()

	deps := (&recordingBake{}).deps()
	deps.Customize = func(ctx context.Context, _ string, steps []Step) ([]Skipped, error) {
		// Only the web UI extract fails, exactly as a Zabbly outage fails it.
		return Apply(ctx, root, steps, &recordingRunner{failOn: uiPackage})
	}

	skipped, err := Bake(t.Context(), p, deps)

	// An optional step is optional: the bake still succeeds. That is the whole
	// reason its failure has to be reported some other way.
	if err != nil {
		t.Fatalf("Bake failed although only an optional step did: %v", err)
	}
	if len(skipped) == 0 {
		t.Fatal("Bake reported nothing skipped although the web UI step failed; " +
			"a caller cannot tell this image from a complete one")
	}
	var found bool
	for _, s := range skipped {
		if strings.Contains(s.Step.Desc, uiPackage) {
			found = true
			if s.Err == nil {
				t.Error("the skipped step carries no reason")
			}
		}
	}
	if !found {
		t.Errorf("the skipped steps %v do not name the web UI step that failed", skipped)
	}
}

// A bake that skips nothing must report nothing, so a caller can print the
// skipped list unconditionally and a clean build stays clean.
func TestBakeReportsNothingSkippedWhenEveryStepRan(t *testing.T) {
	p := planFor(t, t.TempDir(), filepath.Join(t.TempDir(), "guest.qcow2"), 8)
	root := t.TempDir()

	deps := (&recordingBake{}).deps()
	deps.Customize = func(ctx context.Context, _ string, steps []Step) ([]Skipped, error) {
		return Apply(ctx, root, steps, &recordingRunner{})
	}

	skipped, err := Bake(t.Context(), p, deps)
	if err != nil {
		t.Fatalf("Bake: %v", err)
	}
	if len(skipped) != 0 {
		t.Errorf("a clean bake reported %d skipped steps: %v", len(skipped), skipped)
	}
}

// A step skipped before a LATER phase fails is still part of what happened, so
// it must come back with the error rather than be discarded with it. A build
// that fails at compress has still told the operator something about the image
// it was about to compress.
func TestBakeReportsSkippedStepsEvenWhenALaterPhaseFails(t *testing.T) {
	p := planFor(t, t.TempDir(), filepath.Join(t.TempDir(), "guest.qcow2"), 8)
	root := t.TempDir()

	rec := &recordingBake{failOn: string(PhaseCompress)}
	deps := rec.deps()
	deps.Customize = func(ctx context.Context, _ string, steps []Step) ([]Skipped, error) {
		return Apply(ctx, root, steps, &recordingRunner{failOn: uiPackage})
	}

	skipped, err := Bake(t.Context(), p, deps)
	if err == nil {
		t.Fatal("Bake succeeded although the compress phase failed")
	}
	if len(skipped) == 0 {
		t.Error("a failed bake discarded the optional steps it had already skipped")
	}
}
