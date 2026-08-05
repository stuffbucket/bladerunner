package imagebuild

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// recordingBake records the order phases ran in and lets one of them fail, so
// the orchestration can be asserted without root, a device, or a network.
type recordingBake struct {
	order   []string
	failOn  string
	fetched string
	argv    [][]string
	steps   int
	skipped []Skipped
}

func (b *recordingBake) deps() BakeDeps {
	fail := func(phase string) error {
		if b.failOn == phase {
			return errors.New("the " + phase + " phase failed")
		}
		return nil
	}
	return BakeDeps{
		Fetch: func(_ context.Context, r Release, dest string) error {
			b.order = append(b.order, string(PhaseFetch))
			b.fetched = dest
			_ = r
			return fail(string(PhaseFetch))
		},
		Run: func(_ context.Context, argv []string) error {
			// Resize and compress both come through Run; the argv says which.
			phase := string(PhaseResize)
			if slices.Contains(argv, "convert") {
				phase = string(PhaseCompress)
			}
			b.order = append(b.order, phase)
			b.argv = append(b.argv, argv)
			return fail(phase)
		},
		Customize: func(_ context.Context, _ string, steps []Step) ([]Skipped, error) {
			b.order = append(b.order, string(PhaseCustomize))
			b.steps = len(steps)
			return b.skipped, fail(string(PhaseCustomize))
		},
		Publish: func(_, _ string) error {
			b.order = append(b.order, string(PhasePublish))
			return fail(string(PhasePublish))
		},
	}
}

// A bake performs every phase, in the plan's order, against the plan's paths.
func TestBakeRunsEveryPhaseInOrder(t *testing.T) {
	p := planFor(t, t.TempDir(), "/tmp/guest.qcow2", 8)
	rec := &recordingBake{}

	if _, err := Bake(t.Context(), p, rec.deps()); err != nil {
		t.Fatalf("Bake: %v", err)
	}

	want := []string{"fetch", "resize", "customize", "compress", "publish"}
	if !slices.Equal(rec.order, want) {
		t.Errorf("phases ran %v, want %v", rec.order, want)
	}
	if rec.fetched != p.BasePath {
		t.Errorf("fetched to %q, want the plan's base %q", rec.fetched, p.BasePath)
	}
	if rec.steps != len(p.Recipe.Steps()) {
		t.Errorf("customize got %d steps, want the recipe's %d", rec.steps, len(p.Recipe.Steps()))
	}
}

// The first failure stops the bake. A phase that runs after a failed one is
// working on an image whose earlier state was never established — and the
// worst case is publishing it, which is how a half-built image reaches
// production reporting success.
func TestBakeStopsAtTheFirstFailure(t *testing.T) {
	for _, phase := range []string{"fetch", "resize", "customize", "compress"} {
		t.Run(phase, func(t *testing.T) {
			p := planFor(t, t.TempDir(), "/tmp/guest.qcow2", 8)
			rec := &recordingBake{failOn: phase}

			_, err := Bake(t.Context(), p, rec.deps())
			if err == nil {
				t.Fatalf("Bake succeeded although %s failed", phase)
			}
			if !strings.Contains(err.Error(), phase) {
				t.Errorf("error %q does not name the phase that failed", err)
			}
			if got := rec.order[len(rec.order)-1]; got != phase {
				t.Errorf("last phase run was %q, want %q — something ran after the failure", got, phase)
			}
			if slices.Contains(rec.order, "publish") {
				t.Error("the image was published despite an earlier failure")
			}
		})
	}
}

// A bake missing a dependency must be refused before anything runs, not
// partway through with an image already written.
func TestBakeRefusesIncompleteDeps(t *testing.T) {
	p := planFor(t, t.TempDir(), "/tmp/guest.qcow2", 8)
	full := (&recordingBake{}).deps()

	for name, deps := range map[string]BakeDeps{
		"no fetch":     {Run: full.Run, Customize: full.Customize, Publish: full.Publish},
		"no run":       {Fetch: full.Fetch, Customize: full.Customize, Publish: full.Publish},
		"no customize": {Fetch: full.Fetch, Run: full.Run, Publish: full.Publish},
		"no publish":   {Fetch: full.Fetch, Run: full.Run, Customize: full.Customize},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Bake(t.Context(), p, deps); err == nil {
				t.Error("Bake accepted a dependency set it cannot finish with")
			}
		})
	}
}

// Publish is the last phase and it names the partial and the output, so the
// image only takes its final name once it is complete.
func TestBakePublishesTheCompletedPartial(t *testing.T) {
	p := planFor(t, t.TempDir(), "/tmp/guest.qcow2", 8)

	var from, to string
	deps := (&recordingBake{}).deps()
	deps.Publish = func(f, t2 string) error { from, to = f, t2; return nil }

	if _, err := Bake(t.Context(), p, deps); err != nil {
		t.Fatalf("Bake: %v", err)
	}
	if from != p.PartialPath || to != p.OutputPath {
		t.Errorf("published %q -> %q, want %q -> %q", from, to, p.PartialPath, p.OutputPath)
	}
}

// NewBakeDeps must hand Bake a complete dependency set, or Bake refuses before
// running a phase and the failure reads as a programming mistake rather than as
// a missing piece of wiring.
//
// The mechanic is the only part that varies by platform, and it arrives as an
// argument, so this holds everywhere without a build tag.
func TestNewBakeDepsIsComplete(t *testing.T) {
	deps := NewBakeDeps(func(context.Context, string, []Step) ([]Skipped, error) { return nil, nil }, nil)

	if err := deps.validate(); err != nil {
		t.Fatalf("NewBakeDeps returned an incomplete set: %v", err)
	}
	if deps.Fetch == nil || deps.Run == nil || deps.Customize == nil || deps.Publish == nil {
		t.Error("a dependency is nil despite validate passing")
	}
}

// A host with no mechanic must fail before a bake starts, not inside one.
//
// HostMechanic returning an error rather than a mechanic that refuses when
// called is what buys this: the caller cannot reach Bake, so it cannot fetch
// 321 MB and resize it before finding out there was nothing to run.
func TestHostMechanicRefusesBeforeAnyWork(t *testing.T) {
	mechanic, err := HostMechanic(t.TempDir(), nil)

	if runtime.GOOS == "linux" {
		if err != nil {
			t.Fatalf("Linux has the mechanic but HostMechanic refused: %v", err)
		}
		if mechanic == nil {
			t.Error("HostMechanic returned no mechanic and no error")
		}
		return
	}

	if err == nil {
		t.Fatalf("%s has no mechanic but HostMechanic returned one", runtime.GOOS)
	}
	if mechanic != nil {
		t.Error("HostMechanic returned both a mechanic and an error")
	}
	if !errors.Is(err, ErrUnsupportedHost) {
		t.Errorf("error %q does not wrap ErrUnsupportedHost, so callers cannot tell it apart", err)
	}
}

// runHostCommand must fold the command's own output into the error. A qemu-img
// failure reported as "exit status 1" tells a user nothing they can act on.
func TestRunHostCommandReportsWhatTheToolSaid(t *testing.T) {
	err := runHostCommand(t.Context(), []string{"sh", "-c", "echo the-tool-said-this >&2; exit 3"})
	if err == nil {
		t.Fatal("a failing command reported no error")
	}
	if !strings.Contains(err.Error(), "the-tool-said-this") {
		t.Errorf("error %q does not carry the command's own output", err)
	}
}

// An empty argv is a caller bug, not something to hand to exec.
func TestRunHostCommandRefusesAnEmptyArgv(t *testing.T) {
	if err := runHostCommand(t.Context(), nil); err == nil {
		t.Error("an empty command was accepted")
	}
}

// The output's directory must exist before the compress phase writes the
// partial into it. qemu-img does not create parents, so a bake into a path
// whose directory is absent fails AFTER fetching and customizing — several
// minutes and several hundred megabytes in, on a mistake knowable at the start.
//
// The shell build created it; the Go path did not, and this was found by
// running the command a user would run rather than by reading either.
func TestBakeCreatesTheOutputDirectory(t *testing.T) {
	nested := filepath.Join(t.TempDir(), "does", "not", "exist", "guest.qcow2")
	p := planFor(t, t.TempDir(), nested, 8)

	if _, err := Bake(t.Context(), p, (&recordingBake{}).deps()); err != nil {
		t.Fatalf("Bake: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(nested)); err != nil {
		t.Errorf("the bake did not create the output directory: %v", err)
	}
}
