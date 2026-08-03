package vmhost

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stuffbucket/bladerunner/internal/config"
)

// --- Run's own wiring ------------------------------------------------------
//
// The tests below drive Run itself, not the stepStack underneath it (that is
// covered above). They use the two seams on Host — stepsFn and waitReady — so
// no disk is attached, no socket is bound and no VM is started; what is under
// test is the wiring Run adds around the steps: the cancel CAUSE, the startedAt
// stamp, the unconditional teardown, and the hand-off to block.

// stubObserver records the block-phase notifications and lets a test release
// the block from inside Waiting, which is the one point at which Run is
// guaranteed to be sitting in <-ctx.Done().
type stubObserver struct {
	NopObserver
	events    *recorder
	onWaiting func()
}

// Ready implements Observer.
func (o *stubObserver) Ready(*config.Config, string, error) { o.events.log("ready") }

// Waiting implements Observer.
func (o *stubObserver) Waiting(bool) {
	o.events.log("waiting")
	if o.onWaiting != nil {
		o.onWaiting()
	}
}

// Stopping implements Observer.
func (o *stubObserver) Stopping() { o.events.log("stopping") }

// newFakeHost returns a headless Host whose lifecycle is steps and whose
// readiness wait succeeds at once, plus the observer recording into r.
func newFakeHost(t *testing.T, r *recorder, steps ...step) (*Host, *stubObserver) {
	t.Helper()
	t.Setenv(config.ForceHostedImageEnvVar, "")
	t.Setenv(config.ForceDebianImageEnvVar, "")

	h, err := New(flatSpec())
	if err != nil {
		t.Fatal(err)
	}
	// block reads GUI off the resolved config; the real StepConfig is what
	// normally puts it there.
	h.cfg = &config.Config{}
	h.stepsFn = func() []step { return steps }
	h.waitReady = func(context.Context) error { return nil }

	obs := &stubObserver{events: r}
	h.SetObserver(obs)
	return h, obs
}

// blockGrace is how long a Run that has reached the block is given to prove it
// is genuinely blocked (it must NOT return) before the context is released.
const blockGrace = 50 * time.Millisecond

// A clean Run starts every step, hands over to block, STAYS there until the
// context is released, and only then tears the steps back down in exact
// reverse. The event sequence is the assertion: teardown appearing after
// "stopping" is what proves Run went through block rather than bailing out.
func TestRunReachesBlockAndTearsDownOnCancel(t *testing.T) {
	r := &recorder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	waiting := make(chan struct{})
	h, obs := newFakeHost(t, r, fakeStep(r, "one", nil), fakeStep(r, "two", nil))
	obs.onWaiting = func() { close(waiting) }

	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()

	select {
	case <-waiting:
	case err := <-done:
		t.Fatalf("Run() = %v before it ever reached the block", err)
	case <-time.After(time.Second):
		t.Fatal("Run never reached the block")
	}
	// The whole point of block is that the host stays up: a Run that returns
	// here would tear a healthy VM down the instant it finished booting.
	select {
	case err := <-done:
		t.Fatalf("Run() = %v while its context was still live; it must block", err)
	case <-time.After(blockGrace):
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after its context was canceled")
	}

	want := "start:one,start:two,ready,waiting,stopping,stop:two,stop:one"
	if got := joined(r.events); got != want {
		t.Fatalf("Run sequence = %q, want %q", got, want)
	}
}

// A step that fails to start ends the run with that step's error UNCHANGED —
// callers match on the sentinel a step returned — and everything already
// started is unwound exactly once, even though Run's deferred teardown also
// runs after stack.run has already unwound. block is never reached.
func TestRunReturnsTheStepErrorUnchangedAndTearsDownOnce(t *testing.T) {
	r := &recorder{}
	boom := errors.New("step three failed")
	h, _ := newFakeHost(t, r,
		fakeStep(r, "one", nil),
		fakeStep(r, "two", nil),
		fakeStep(r, "three", boom),
		fakeStep(r, "four", nil),
	)

	err := h.Run(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("Run() = %v, want %v", err, boom)
	}
	if got := err.Error(); got != boom.Error() {
		t.Errorf("Run() = %q, want the step's error unchanged (%q)", got, boom.Error())
	}
	// Each stop appears once: Run's defer must not stop a second time what
	// stack.run already unwound. No "ready"/"waiting"/"stopping": block never ran.
	want := "start:one,start:two,start:three,stop:two,stop:one"
	if got := joined(r.events); got != want {
		t.Fatalf("Run sequence = %q, want %q", got, want)
	}
}

// A release from inside this process must be recorded as the CAUSE on the
// context Run hands its steps, so a wait that ends early can say the host was
// asked to stop instead of reporting a bare "context canceled" — which reads
// exactly like its own budget expiring.
func TestRunRecordsStopRequestedAsTheCancelCause(t *testing.T) {
	r := &recorder{}
	// The step context is handed out over a channel rather than assigned to an
	// outer variable, which keeps it a value under test instead of a context
	// stored in a struct (fatcontext).
	captured := make(chan context.Context, 1)
	capture := step{
		name: "capture",
		start: func(ctx context.Context) error {
			captured <- ctx
			r.log("start:capture")
			return nil
		},
		stop: func() error { r.log("stop:capture"); return nil },
	}

	h, obs := newFakeHost(t, r, capture)
	// stop() is documented as safe from any goroutine — a control-socket
	// handler and a DiskArbitration callback are both where it comes from.
	obs.onWaiting = func() {
		done := make(chan struct{})
		go func() { defer close(done); h.stop() }()
		<-done
	}

	if err := h.Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	var stepCtx context.Context
	select {
	case stepCtx = <-captured:
	default:
		t.Fatal("no step ran, so no run context was captured")
	}
	if err := stepCtx.Err(); err == nil {
		t.Fatal("the run context was never released")
	}
	if cause := context.Cause(stepCtx); !errors.Is(cause, ErrStopRequested) {
		t.Fatalf("context.Cause = %v, want %v", cause, ErrStopRequested)
	}
}

// startedAt is what the registry entry publishes as this instance's start time,
// so it must be stamped before any step runs — not after the last one.
func TestRunStampsStartedAtBeforeTheFirstStep(t *testing.T) {
	r := &recorder{}
	var h *Host
	var seen time.Time
	probe := step{
		name: "probe",
		start: func(context.Context) error {
			seen = h.startedAt
			r.log("start:probe")
			return nil
		},
	}

	before := time.Now()
	var obs *stubObserver
	h, obs = newFakeHost(t, r, probe)
	obs.onWaiting = h.stop

	if err := h.Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if seen.IsZero() {
		t.Fatal("startedAt was still zero when the first step ran")
	}
	if seen.Before(before) || seen.After(time.Now()) {
		t.Errorf("startedAt = %v, want a stamp taken during this test", seen)
	}
	if got := h.Info().StartedAt; !got.Equal(seen) {
		t.Errorf("Info().StartedAt = %v, want the stamp the first step saw (%v)", got, seen)
	}
}

// teardown marks the guest stopped BEFORE it unwinds anything. That ordering is
// load-bearing: an unmount-approval callback that fires while the steps are
// coming down must approve — there is nothing left to protect — instead of
// vetoing this host's own cartridge detach and deadlocking the eject it is
// performing.
func TestRunMarksTheGuestStoppedBeforeUnwinding(t *testing.T) {
	r := &recorder{}
	var stoppedDuringUnwind bool
	var h *Host
	probe := step{
		name:  "probe",
		start: func(context.Context) error { r.log("start:probe"); return nil },
		stop: func() error {
			stoppedDuringUnwind = h.guestStopped.Load()
			r.log("stop:probe")
			return nil
		},
	}

	var obs *stubObserver
	h, obs = newFakeHost(t, r, probe)
	if h.guestStopped.Load() {
		t.Fatal("guestStopped is set before Run even starts")
	}
	obs.onWaiting = h.stop

	if err := h.Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if !strings.Contains(joined(r.events), "stop:probe") {
		t.Fatalf("teardown never ran: %q", joined(r.events))
	}
	if !stoppedDuringUnwind {
		t.Error("a step's stop ran while guestStopped was still false; an unmount approval would veto our own detach")
	}
	// decideUnmount is the rule that ordering feeds; state it at the Run level.
	if d := decideUnmount(h.unmountState()); d.StartDrain {
		t.Error("an unmount request during teardown would start a second drain")
	}
}

// Run owns the main thread, but Drain, Info, Runner and UnmountProtection are
// documented as safe from any goroutine — the control-socket handler goroutine
// (started by StepServe) and the DiskArbitration dispatch queue (registered by
// StepUnmountVeto) both call them while later steps are still bringing the
// instance up. This reproduces that shape: readers on their own goroutines,
// writers on the goroutine driving Run, overlapping. Run it under -race.
func TestRunIsSafeWithConcurrentDrainAndInfo(t *testing.T) {
	r := &recorder{}
	var h *Host
	var wg sync.WaitGroup
	var stopReading atomic.Bool

	// writes is large enough that the readers below are demonstrably still in
	// their loop while the writers run; ready is what pins that overlap down
	// rather than leaving it to scheduling luck.
	const writes = 2000
	const readers = 2
	ready := make(chan struct{}, readers)

	// StepServe's stand-in: the goroutines a step leaves behind, which then call
	// the goroutine-safe accessors for as long as the host lives.
	serve := step{
		name: "serve",
		start: func(ctx context.Context) error {
			r.log("start:serve")
			for range readers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					ready <- struct{}{}
					for !stopReading.Load() {
						_ = h.Info()
						_ = h.UnmountProtection()
						_ = h.Runner()
						_ = h.Drain(ctx, time.Millisecond)
					}
				}()
			}
			return nil
		},
		stop: func() error { r.log("stop:serve"); return nil },
	}

	// A later step publishing what those readers read — the veto outcome
	// (startUnmountWatch) and the runner (startRunner) — while they read it.
	publish := step{
		name: "publish",
		start: func(context.Context) error {
			r.log("start:publish")
			for range readers {
				<-ready
			}
			for range writes {
				h.setUnmountProtection(UnprotectedNoDevNode)
				h.setRunner(nil)
			}
			stopReading.Store(true)
			return nil
		},
	}

	var obs *stubObserver
	h, obs = newFakeHost(t, r, serve, publish)
	obs.onWaiting = func() {
		wg.Wait()
		h.stop()
	}

	if err := h.Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if got := h.UnmountProtection(); got != UnprotectedNoDevNode {
		t.Errorf("UnmountProtection() = %v, want %v", got, UnprotectedNoDevNode)
	}
	if !strings.Contains(joined(r.events), "stop:serve") {
		t.Fatalf("teardown did not run: %q", joined(r.events))
	}
}

// The seams must be inert in production: New sets neither, and with both unset
// Run drives the real fourteen-step lifecycle. This is the test that keeps the
// fields honest — if they ever defaulted to anything but the real thing, every
// test above would be measuring a fiction.
func TestNewLeavesTheTestSeamsUnset(t *testing.T) {
	t.Setenv(config.ForceHostedImageEnvVar, "")
	t.Setenv(config.ForceDebianImageEnvVar, "")

	h, err := New(flatSpec())
	if err != nil {
		t.Fatal(err)
	}
	if h.stepsFn != nil {
		t.Error("New installed a stepsFn; production must resolve to Host.steps")
	}
	if h.waitReady != nil {
		t.Error("New installed a waitReady; production must resolve to waitForGuestReady")
	}
	if h.stopVM != nil {
		t.Error("New installed a stopVM; production must resolve to the live runner")
	}

	// With the seam unset, stopGuest must take the production branch: a Host
	// that never constructed a runner has nothing to stop and says so quietly,
	// because a boot can fail long before the VM exists and teardown still has
	// to reach the cartridge detach.
	if err := h.stopGuest(context.Background(), DefaultDrainTimeout); err != nil {
		t.Errorf("stopGuest with no runner = %v, want nil", err)
	}

	names := func(steps []step) string {
		out := make([]string, 0, len(steps))
		for _, s := range steps {
			out = append(out, s.name)
		}
		return strings.Join(out, ",")
	}
	if got, want := names(h.lifecycleSteps()), names(h.steps()); got != want {
		t.Fatalf("lifecycleSteps() = %q, want the real lifecycle %q", got, want)
	}
}
