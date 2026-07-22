package converge

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// probeFunc builds a Probe returning a fixed ok and counting calls.
func countingProbe(ok bool, calls *int) func(context.Context) (bool, error) {
	return func(context.Context) (bool, error) {
		*calls++
		return ok, nil
	}
}

// TestShortCircuitSkipsHigher: a false lower invariant skips every higher one
// (their probes must not even run).
func TestShortCircuitSkipsHigher(t *testing.T) {
	var lowCalls, highCalls int
	invs := []Invariant{
		{Name: "low", Probe: countingProbe(false, &lowCalls)},
		{Name: "high", Probe: countingProbe(true, &highCalls)},
	}
	c := NewController(invs, NewScheduler(), nil)

	results := c.Tick(context.Background())
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if lowCalls != 1 {
		t.Fatalf("low probe calls = %d, want 1", lowCalls)
	}
	if highCalls != 0 {
		t.Fatalf("high probe calls = %d, want 0 (short-circuited)", highCalls)
	}
	if results[0].OK {
		t.Error("low result should be false")
	}
	if !results[1].Skipped {
		t.Error("high result should be Skipped")
	}
	if Converged(results) {
		t.Error("Converged should be false when a probe failed")
	}
}

// TestHeavyGatedToSteady: a Heavy invariant runs only in the Steady tier.
func TestHeavyGatedToSteady(t *testing.T) {
	var cheapCalls, heavyCalls int
	invs := []Invariant{
		{Name: "cheap", Probe: countingProbe(true, &cheapCalls)},
		{Name: "heavy", Heavy: true, Probe: countingProbe(true, &heavyCalls)},
	}

	// Not steady (Critical): heavy is skipped.
	sched := NewScheduler()
	c := NewController(invs, sched, nil)
	res := c.Tick(context.Background())
	if heavyCalls != 0 {
		t.Fatalf("heavy ran outside Steady: calls = %d", heavyCalls)
	}
	if !res[1].Skipped {
		t.Error("heavy result should be Skipped outside Steady")
	}

	// Force Steady and tick again: heavy runs.
	sched.tier = Steady
	c.Tick(context.Background())
	if heavyCalls != 1 {
		t.Fatalf("heavy did not run in Steady: calls = %d", heavyCalls)
	}
}

// TestHeavyNotRunAfterShortCircuitInSteady: even in Steady, a Heavy probe is
// skipped if a cheaper invariant ahead of it failed.
func TestHeavyNotRunAfterShortCircuitInSteady(t *testing.T) {
	var heavyCalls int
	invs := []Invariant{
		{Name: "cheap", Probe: func(context.Context) (bool, error) { return false, nil }},
		{Name: "heavy", Heavy: true, Probe: countingProbe(true, &heavyCalls)},
	}
	sched := NewScheduler()
	sched.tier = Steady
	c := NewController(invs, sched, nil)
	c.Tick(context.Background())
	if heavyCalls != 0 {
		t.Fatalf("heavy ran after short-circuit in Steady: calls = %d", heavyCalls)
	}
}

// TestSingleFlight: a Tick arriving while a prior Tick is still running is
// skipped (returns nil) rather than overlapping.
func TestSingleFlight(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce sync.Once
	invs := []Invariant{{
		Name: "slow",
		Probe: func(context.Context) (bool, error) {
			// Only the first call blocks (to hold the single-flight guard); later
			// calls return immediately so re-ticking after completion is cheap.
			enterOnce.Do(func() {
				close(entered)
				<-release
			})
			return true, nil
		},
	}}
	c := NewController(invs, NewScheduler(), nil)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.Tick(context.Background())
	}()

	<-entered // first Tick is now inside the probe, holding the single-flight guard.
	if got := c.Tick(context.Background()); got != nil {
		t.Fatalf("overlapping Tick returned %v, want nil (skipped)", got)
	}
	close(release)
	wg.Wait()

	// After the first completes, a fresh Tick runs again.
	if got := c.Tick(context.Background()); got == nil {
		t.Fatal("Tick after completion returned nil, want a result")
	}
}

// TestReconvergeBoundedThenEscalate: a failing invariant with a Reconverge is
// retried up to MaxHostAttempts, then escalates exactly once.
func TestReconvergeBoundedThenEscalate(t *testing.T) {
	var reconverges int
	var escalations int
	invs := []Invariant{{
		Name:  "fixable",
		Probe: func(context.Context) (bool, error) { return false, nil },
		Reconverge: func(context.Context) error {
			reconverges++
			return nil
		},
	}}
	c := NewController(invs, NewScheduler(), func(string) { escalations++ })

	// MaxHostAttempts ticks each attempt a reconverge; none escalate yet.
	for i := 0; i < MaxHostAttempts; i++ {
		res := c.Tick(context.Background())
		if !res[0].Reconverged {
			t.Fatalf("tick %d: expected Reconverged", i)
		}
		if !Reconverging(res) {
			t.Fatalf("tick %d: Reconverging should be true", i)
		}
	}
	if reconverges != MaxHostAttempts {
		t.Fatalf("reconverges = %d, want %d", reconverges, MaxHostAttempts)
	}
	if escalations != 0 {
		t.Fatalf("escalations = %d before exhaustion, want 0", escalations)
	}

	// Next tick exhausts attempts -> escalate once.
	res := c.Tick(context.Background())
	if !res[0].Escalated {
		t.Fatal("expected Escalated after exhausting attempts")
	}
	if escalations != 1 {
		t.Fatalf("escalations = %d, want 1", escalations)
	}
	// Further ticks do not re-escalate (latched).
	c.Tick(context.Background())
	if escalations != 1 {
		t.Fatalf("escalations = %d after extra tick, want 1 (latched)", escalations)
	}
}

// TestObserveOnlyEscalatesOnce: an invariant with nil Reconverge (observe-only)
// escalates once on failure and re-arms after recovery.
func TestObserveOnlyEscalatesOnce(t *testing.T) {
	var escalations int
	ok := false
	invs := []Invariant{{
		Name:  "observe-only",
		Probe: func(context.Context) (bool, error) { return ok, nil },
	}}
	c := NewController(invs, NewScheduler(), func(string) { escalations++ })

	c.Tick(context.Background())
	c.Tick(context.Background())
	if escalations != 1 {
		t.Fatalf("observe-only escalations = %d, want 1 (latched)", escalations)
	}

	// Recover, then fail again -> escalates a second time (latch cleared on OK).
	ok = true
	c.Tick(context.Background())
	ok = false
	c.Tick(context.Background())
	if escalations != 2 {
		t.Fatalf("escalations after recover+refail = %d, want 2", escalations)
	}
}

// TestRecoveryClearsAttempts: a probe that fails then succeeds resets its
// attempt counter, so a later failure gets a fresh MaxHostAttempts budget.
func TestRecoveryClearsAttempts(t *testing.T) {
	var reconverges int
	ok := false
	invs := []Invariant{{
		Name:       "fixable",
		Probe:      func(context.Context) (bool, error) { return ok, nil },
		Reconverge: func(context.Context) error { reconverges++; return nil },
	}}
	c := NewController(invs, NewScheduler(), nil)

	c.Tick(context.Background()) // 1 reconverge attempt
	ok = true
	c.Tick(context.Background()) // recovers, clears attempts
	ok = false
	c.Tick(context.Background()) // fresh budget: another reconverge attempt
	if reconverges != 2 {
		t.Fatalf("reconverges = %d, want 2 (budget reset on recovery)", reconverges)
	}
}

// TestProbeErrorTreatedAsNotOK: a probe returning an error is not converged and
// its error surfaces on the result.
func TestProbeErrorTreatedAsNotOK(t *testing.T) {
	sentinel := errors.New("probe boom")
	invs := []Invariant{{
		Name:  "erroring",
		Probe: func(context.Context) (bool, error) { return false, sentinel },
	}}
	c := NewController(invs, NewScheduler(), nil)
	res := c.Tick(context.Background())
	if !errors.Is(res[0].Err, sentinel) {
		t.Fatalf("result err = %v, want %v", res[0].Err, sentinel)
	}
	if Converged(res) {
		t.Error("Converged should be false on probe error")
	}
}

// TestConvergedIgnoresSkipped: a Heavy-gated (Skipped) invariant does not count
// against convergence.
func TestConvergedIgnoresSkipped(t *testing.T) {
	invs := []Invariant{
		{Name: "cheap", Probe: func(context.Context) (bool, error) { return true, nil }},
		{Name: "heavy", Heavy: true, Probe: func(context.Context) (bool, error) { return true, nil }},
	}
	c := NewController(invs, NewScheduler(), nil) // Critical: heavy skipped
	res := c.Tick(context.Background())
	if !res[1].Skipped {
		t.Fatal("setup: heavy should be Skipped")
	}
	if !Converged(res) {
		t.Error("Converged should be true when the only non-OK entry is a Skipped heavy probe")
	}
}

// TestProbeTimeoutApplied: the per-probe context carries a deadline.
func TestProbeTimeoutApplied(t *testing.T) {
	var hadDeadline bool
	invs := []Invariant{{
		Name: "deadline",
		Probe: func(ctx context.Context) (bool, error) {
			_, hadDeadline = ctx.Deadline()
			return true, nil
		},
	}}
	c := NewController(invs, NewScheduler(), nil)
	c.timeout = 50 * time.Millisecond
	c.Tick(context.Background())
	if !hadDeadline {
		t.Fatal("probe context should carry a deadline")
	}
}
