package vmhost

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stuffbucket/bladerunner/internal/diskarb"
	"github.com/stuffbucket/bladerunner/internal/instance"
)

// callbackBudget is how long a test waits for an unmount-approval callback to
// return. It is generous by test standards and still two orders of magnitude
// below the drain budget, so a callback that waits for the drain fails here.
const callbackBudget = 2 * time.Second

// decideUnmount is total over its two inputs, so the table below is the whole
// specification: enumerate both bits, assert the dissent AND whether this
// callback owns the drain.
func TestDecideUnmountIsExhaustive(t *testing.T) {
	cases := []struct {
		name       string
		state      unmountState
		wantDeny   bool
		wantReason string
		wantDrain  bool
	}{
		{
			name:     "running: veto and start the drain",
			state:    unmountState{Stopped: false, Draining: false},
			wantDeny: true, wantReason: unmountDenyReason, wantDrain: true,
		},
		{
			name:     "draining: veto but do not start a second drain",
			state:    unmountState{Stopped: false, Draining: true},
			wantDeny: true, wantReason: unmountDenyReason, wantDrain: false,
		},
		{
			name:     "stopped: approve",
			state:    unmountState{Stopped: true, Draining: false},
			wantDeny: false, wantDrain: false,
		},
		{
			name:     "stopped while a drain is still unwinding: approve",
			state:    unmountState{Stopped: true, Draining: true},
			wantDeny: false, wantDrain: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideUnmount(tc.state)
			if got.Dissent.Deny != tc.wantDeny {
				t.Fatalf("Deny = %v, want %v", got.Dissent.Deny, tc.wantDeny)
			}
			if got.Dissent.Reason != tc.wantReason {
				t.Fatalf("Reason = %q, want %q", got.Dissent.Reason, tc.wantReason)
			}
			if got.StartDrain != tc.wantDrain {
				t.Fatalf("StartDrain = %v, want %v", got.StartDrain, tc.wantDrain)
			}
			if !tc.wantDeny && got.StartDrain {
				t.Fatal("an approving decision must never also start a drain")
			}
		})
	}
}

// The reason string is what Finder shows the user, and it must be the same
// sentence the menubar shows through the bootstage detail.
func TestUnmountDenyReasonIsUserFacing(t *testing.T) {
	if unmountDenyReason == "" {
		t.Fatal("the deny reason must not be empty: Finder shows it verbatim")
	}
	if got := diskarb.Deny(unmountDenyReason); got.Reason != unmountDenyReason {
		t.Fatalf("Deny(%q).Reason = %q", unmountDenyReason, got.Reason)
	}
}

// newTestHost builds a Host that has never run, which is the state the veto has
// to cope with when an eject arrives during startup.
func newTestHost(t *testing.T) *Host {
	t.Helper()
	host, err := New(Spec{Kind: instance.KindFlat, Name: "veto-test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return host
}

// A running instance vetoes the eject and starts exactly one drain, however
// many callbacks DiskArbitration delivers (Finder retries, and one callback
// arrives per slice of the disk).
func TestOnUnmountApprovalStartsTheDrainExactlyOnce(t *testing.T) {
	host := newTestHost(t)

	var kicks atomic.Int64
	release := make(chan struct{})
	host.drainKick = func() {
		kicks.Add(1)
		<-release // a real drain can take the whole budget
	}
	t.Cleanup(func() { close(release) })

	const callers = 16
	var wg sync.WaitGroup
	denied := make([]bool, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			denied[i] = host.onUnmountApproval(diskarb.DiskInfo{BSDName: "disk9s1"}).Deny
		}()
	}
	waitFor(t, &wg, callbackBudget, "unmount-approval callbacks did not return while the drain was in flight")

	for i, d := range denied {
		if !d {
			t.Fatalf("caller %d was approved while the VM was running", i)
		}
	}
	// The drain goroutine may not have been scheduled yet, but it must never
	// run more than once.
	waitForCount(t, &kicks, 1, callbackBudget)
	if got := kicks.Load(); got != 1 {
		t.Fatalf("drain started %d times, want exactly 1", got)
	}
}

// The crux of the callback contract: the answer must come back immediately even
// though the drain it triggered is blocked. A callback that waited for the
// drain would hang Finder for up to a minute.
func TestOnUnmountApprovalReturnsWithoutWaitingForTheDrain(t *testing.T) {
	host := newTestHost(t)

	blocked := make(chan struct{})
	release := make(chan struct{})
	host.drainKick = func() {
		close(blocked)
		<-release
	}
	t.Cleanup(func() { close(release) })

	done := make(chan diskarb.Dissent, 1)
	go func() { done <- host.onUnmountApproval(diskarb.DiskInfo{BSDName: "disk9s1"}) }()

	select {
	case got := <-done:
		if !got.Deny {
			t.Fatal("a running VM's volume must not be released")
		}
	case <-time.After(callbackBudget):
		t.Fatal("the approval callback blocked on the drain")
	}

	select {
	case <-blocked:
	case <-time.After(callbackBudget):
		t.Fatal("the drain was never started")
	}
}

// Once the guest is down there is nothing left to protect, so the eject is
// approved and no drain is started.
func TestOnUnmountApprovalApprovesAStoppedGuest(t *testing.T) {
	host := newTestHost(t)
	host.drainKick = func() { t.Error("a stopped guest must not trigger a drain") }
	host.guestStopped.Store(true)

	if got := host.onUnmountApproval(diskarb.DiskInfo{BSDName: "disk9s1"}); got.Deny {
		t.Fatalf("stopped guest was denied: %+v", got)
	}
}

// Teardown implies "stopped": an approval callback that fires while the steps
// are unwinding must not veto our own detach.
func TestTeardownMarksTheGuestStopped(t *testing.T) {
	host := newTestHost(t)
	if host.unmountState().Stopped {
		t.Fatal("a fresh Host must not claim the guest is stopped")
	}
	host.teardown()
	if !host.unmountState().Stopped {
		t.Fatal("teardown must mark the guest stopped")
	}
	if got := host.onUnmountApproval(diskarb.DiskInfo{}); got.Deny {
		t.Fatalf("teardown-time eject was denied: %+v", got)
	}
}

// Draining a Host that never started a VM reports ErrNotStarted and marks the
// guest stopped: there is no VM writing to the volume, so a later eject must be
// approved rather than vetoed forever.
func TestDrainWithoutARunnerMarksTheGuestStopped(t *testing.T) {
	host := newTestHost(t)
	if err := host.Drain(context.Background(), time.Second); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("Drain = %v, want ErrNotStarted", err)
	}
	if !host.unmountState().Stopped {
		t.Fatal("a drain with nothing to drain must leave the guest marked stopped")
	}
}

// A non-cartridge instance has no device node to watch, so registration is a
// no-op on every platform — and it must not fail the start.
func TestStartUnmountWatchSkipsNonCartridgeInstances(t *testing.T) {
	host := newTestHost(t)
	if err := host.startUnmountWatch(); err != nil {
		t.Fatalf("startUnmountWatch: %v", err)
	}
	if host.unmountCancel != nil {
		t.Fatal("a flat instance must not register an unmount watcher")
	}
	if err := host.stopUnmountWatch(); err != nil {
		t.Fatalf("stopUnmountWatch: %v", err)
	}
}

// Unregistering is idempotent: teardown runs it, and a failed start may have
// run it already.
func TestStopUnmountWatchIsIdempotent(t *testing.T) {
	host := newTestHost(t)
	var cancels atomic.Int64
	host.unmountCancel = func() error {
		cancels.Add(1)
		return nil
	}
	for range 3 {
		if err := host.stopUnmountWatch(); err != nil {
			t.Fatalf("stopUnmountWatch: %v", err)
		}
	}
	if got := cancels.Load(); got != 1 {
		t.Fatalf("cancel ran %d times, want exactly 1", got)
	}
}

// drainTimeout prefers the Spec's budget and falls back to the package default.
func TestDrainTimeoutFallsBackToTheDefault(t *testing.T) {
	host := newTestHost(t)
	if got := host.drainTimeout(); got != DefaultDrainTimeout {
		t.Fatalf("drainTimeout() = %v, want %v", got, DefaultDrainTimeout)
	}

	withSpec, err := New(Spec{Kind: instance.KindFlat, DrainTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := withSpec.drainTimeout(); got != 5*time.Second {
		t.Fatalf("drainTimeout() = %v, want 5s", got)
	}
}

// waitFor bounds a WaitGroup so a hung callback fails the test instead of
// hanging the suite.
func waitFor(t *testing.T, wg *sync.WaitGroup, budget time.Duration, msg string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(budget):
		t.Fatal(msg)
	}
}

// waitForCount polls until the counter reaches want or the budget expires. It
// does not fail on timeout: the caller asserts the final value, so an
// overshooting counter is reported as "ran N times" rather than "timed out".
func waitForCount(t *testing.T, counter *atomic.Int64, want int64, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if counter.Load() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
}
