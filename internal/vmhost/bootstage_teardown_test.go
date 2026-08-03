package vmhost

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/bootstage"
	"github.com/stuffbucket/bladerunner/internal/config"
)

// bootStageHost builds the smallest Host that can run the boot-stage step: the
// step reads nothing but cfg.VMDir, which is the instance's own directory.
func bootStageHost(t *testing.T) (*Host, string) {
	t.Helper()
	dir := t.TempDir()
	return &Host{cfg: &config.Config{VMDir: dir}}, dir
}

// bootStageSteps is the real boot-stage step plus one that starts after it, so
// teardown unwinds the later step first and the boot-stage step last —
// the production ordering (StepBootStage is followed by StepVM and
// StepWebProxy).
func bootStageSteps(h *Host) []step {
	return []step{
		{name: StepBootStage, start: noCtx(h.startBootStage), stop: h.stopBootStage},
		{name: "later", start: noCtx(func() error { return nil }), stop: func() error { return nil }},
	}
}

// A forced stop warns that the guest filesystem may need a check, and that
// warning has to still be readable once teardown has finished — it is the whole
// point of publishing it. AGENTS.md section 8 governs this: a power cut can
// leave the user's filesystem dirty, and a warning nobody can read is the same
// as no warning.
//
// The sequence is the production one for a vetoed eject that could not drain:
// the boot-stage step publishes on the way up, the drain forces the stop and
// records the warning, and only then does teardown unwind. Before the
// correction, stopBootStage deleted the file here and the warning survived for
// the length of a teardown.
func TestTeardownKeepsTheForcedStopWarningReadable(t *testing.T) {
	h, dir := bootStageHost(t)

	var stack stepStack
	if err := stack.run(context.Background(), bootStageSteps(h), nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	// drainForUnmount's failure branch: force the stop and warn about the disk.
	if err := bootstage.NewReporter(dir).Forced(bootstage.DetailForced); err != nil {
		t.Fatalf("publish forced: %v", err)
	}

	stack.teardown(nil)

	st, ok := bootstage.Read(dir)
	if !ok {
		t.Fatal("teardown deleted the forced-stop warning; a dirty guest filesystem is now unreported")
	}
	if st.Stage != bootstage.Forced {
		t.Errorf("stage = %q, want %q", st.Stage, bootstage.Forced)
	}
	if st.Detail != bootstage.DetailForced {
		t.Errorf("detail = %q, want the dirty-filesystem warning %q", st.Detail, bootstage.DetailForced)
	}
	if got := bootstage.Describe(st); !strings.Contains(got, "may need a check") {
		t.Errorf("Describe = %q, want it to still warn that the disk may need a check", got)
	}
}

// The pair, asserted together on purpose. Skipping Clear for a shutdown
// terminal is the correction; still clearing a boot-phase stage is the
// behavior the correction must not trade away. Without the second half the
// fix could regress into never clearing, which would leave a stale "Ready"
// on the menubar after the VM is gone.
func TestStopBootStageClearsTheBootPhaseAndKeepsAShutdownTerminal(t *testing.T) {
	cases := []struct {
		name    string
		stage   bootstage.Stage
		detail  string
		wantKep bool
	}{
		{name: "boot phase in progress is cleared", stage: bootstage.Boot, wantKep: false},
		{name: "boot phase ready is cleared", stage: bootstage.Ready, wantKep: false},
		{name: "boot phase failed is cleared", stage: bootstage.Failed, wantKep: false},
		{name: "shutdown in progress is cleared", stage: bootstage.Ejecting, wantKep: false},
		{name: "shutdown stopped is kept", stage: bootstage.Stopped, wantKep: true},
		{name: "shutdown forced is kept", stage: bootstage.Forced, detail: bootstage.DetailForced, wantKep: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, dir := bootStageHost(t)
			if err := bootstage.WriteState(dir, bootstage.State{Stage: tc.stage, Detail: tc.detail}); err != nil {
				t.Fatalf("seed %q: %v", tc.stage, err)
			}

			if err := h.stopBootStage(); err != nil {
				t.Fatalf("stopBootStage: %v", err)
			}

			st, ok := bootstage.Read(dir)
			if ok != tc.wantKep {
				t.Fatalf("after teardown with %q published: file kept = %v, want %v", tc.stage, ok, tc.wantKep)
			}
			if tc.wantKep && st.Stage != tc.stage {
				t.Errorf("kept stage = %q, want %q", st.Stage, tc.stage)
			}
		})
	}
}

// cartridgeStep is a stand-in for StepCartridge, whose stop is the detach. It
// is the last step to unwind, so its outcome decides whether the cartridge is
// really released by the time teardown returns.
func cartridgeStep(stopErr error) step {
	return step{
		name:  StepCartridge,
		start: noCtx(func() error { return nil }),
		stop:  func() error { return stopErr },
	}
}

// teardownAfterDrain runs a teardown whose stack is [cartridge, boot-stage] —
// the production order, so the boot-stage step unwinds first and the detach
// last — with seed already published by the drain.
func teardownAfterDrain(t *testing.T, seed func(*bootstage.Reporter) error, detachErr error) (bootstage.State, bool) {
	t.Helper()
	h, dir := bootStageHost(t)
	h.obs = NopObserver{}

	steps := []step{
		cartridgeStep(detachErr),
		{name: StepBootStage, start: noCtx(h.startBootStage), stop: h.stopBootStage},
	}
	if err := h.stack.run(context.Background(), steps, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := seed(bootstage.NewReporter(dir)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	h.teardown()

	st, ok := bootstage.Read(dir)
	return st, ok
}

// The triple. A spin-down has three outcomes and each publishes something
// different, so they are asserted together: the clean terminal is only honest
// AFTER the detach, and the two failure paths must not produce it.
func TestTeardownPublishesTheCleanTerminalOnlyAfterTheDetach(t *testing.T) {
	t.Run("clean eject publishes the terminal once the cartridge is released", func(t *testing.T) {
		st, ok := teardownAfterDrain(t, func(r *bootstage.Reporter) error { return r.Ejecting("") }, nil)
		if !ok || st.Stage != bootstage.Stopped {
			t.Fatalf("stage = %q (present=%v), want %q once the detach succeeded", st.Stage, ok, bootstage.Stopped)
		}
	})

	t.Run("failed drain still publishes forced", func(t *testing.T) {
		st, ok := teardownAfterDrain(t, func(r *bootstage.Reporter) error {
			return r.Forced(bootstage.DetailForced)
		}, nil)
		if !ok || st.Stage != bootstage.Forced {
			t.Fatalf("stage = %q (present=%v), want %q; a forced stop must not be overwritten", st.Stage, ok, bootstage.Forced)
		}
		if st.Detail != bootstage.DetailForced {
			t.Errorf("detail = %q, want the dirty-filesystem warning", st.Detail)
		}
	})

	t.Run("failed detach publishes neither", func(t *testing.T) {
		st, ok := teardownAfterDrain(t, func(r *bootstage.Reporter) error { return r.Ejecting("") },
			errors.New("resource busy"))
		if ok && st.Stage == bootstage.Stopped {
			t.Fatal("teardown said Stopped after the detach FAILED; the cartridge is still attached and this reads as safe to pull")
		}
	})
}
