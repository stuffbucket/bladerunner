package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stuffbucket/bladerunner/internal/control"
	"github.com/stuffbucket/bladerunner/internal/vmhost"
)

// Both `br start` and `br vmd` end in host.Run, and both route its error
// through explainHostError. The two sentinels it matches used to be returned
// and never matched anywhere, which meant the friendly message they exist for
// did not exist: the user saw the raw wrapped text and no way out of it.

// A locked instance must name the process holding it — that PID is what
// `br instances` shows and what the user needs to act — and must point at the
// commands that resolve it, without breaking errors.Is for anyone above.
func TestExplainHostErrorNamesTheLockHolder(t *testing.T) {
	const pid = 4242
	locked := fmt.Errorf("%w: pid %d holds /state/disks/incus/control.lock", control.ErrInstanceLocked, pid)

	got := explainHostError(locked)
	if !errors.Is(got, control.ErrInstanceLocked) {
		t.Fatal("the sentinel must stay wrapped, or a caller above can no longer match it")
	}
	msg := got.Error()
	if !strings.Contains(msg, fmt.Sprint(pid)) {
		t.Errorf("message does not name the holding pid %d: %s", pid, msg)
	}
	if !strings.Contains(msg, "br instances") {
		t.Errorf("message does not point at 'br instances': %s", msg)
	}
	if !strings.Contains(msg, control.ErrInstanceLocked.Error()) {
		t.Errorf("message dropped the underlying error: %s", msg)
	}
}

// The contended branch of the lock names no holder at all, so the hint has to
// degrade to "ask br instances" rather than print a bogus pid.
func TestExplainHostErrorWithoutAHolderPID(t *testing.T) {
	locked := fmt.Errorf("%w: /state/disks/incus/control.lock is contended", control.ErrInstanceLocked)

	msg := explainHostError(locked).Error()
	if !strings.Contains(msg, "br instances") {
		t.Errorf("message does not point at 'br instances': %s", msg)
	}
	if strings.Contains(msg, "process 0") {
		t.Errorf("message invented a holder: %s", msg)
	}
}

// An instance whose control socket already answers is a different fact with a
// different remedy, and it must be recognized too.
func TestExplainHostErrorOnAnAlreadyRunningInstance(t *testing.T) {
	got := explainHostError(fmt.Errorf("start: %w", vmhost.ErrAlreadyRunning))
	if !errors.Is(got, vmhost.ErrAlreadyRunning) {
		t.Fatal("the sentinel must stay wrapped")
	}
	msg := got.Error()
	for _, want := range []string{"br instances", "br stop"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message does not mention %q: %s", want, msg)
		}
	}
}

// Everything else passes through untouched — including nil, which is the
// overwhelmingly common outcome of a clean shutdown.
func TestExplainHostErrorPassesEverythingElseThrough(t *testing.T) {
	if got := explainHostError(nil); got != nil {
		t.Fatalf("explainHostError(nil) = %v, want nil", got)
	}
	other := errors.New("the VM caught fire")
	if got := explainHostError(other); !errors.Is(got, other) || got.Error() != other.Error() {
		t.Fatalf("explainHostError(%v) = %v, want it unchanged", other, got)
	}
}

// lockHolderPID reads a number out of a message, so its edge cases are worth
// stating: no marker, no digits, a zero pid, and the real shape.
func TestLockHolderPID(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
		ok   bool
	}{
		{name: "the real shape", err: errors.New("holds this instance: pid 991 holds /x/control.lock"), want: 991, ok: true},
		{name: "no marker", err: errors.New("/x/control.lock is contended"), ok: false},
		{name: "marker without digits", err: errors.New("pid unknown holds /x/control.lock"), ok: false},
		{name: "zero is not a process", err: errors.New("pid 0 holds /x/control.lock"), ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := lockHolderPID(tt.err)
			if ok != tt.ok || got != tt.want {
				t.Errorf("lockHolderPID() = (%d, %v), want (%d, %v)", got, ok, tt.want, tt.ok)
			}
		})
	}
}

// --- the signal that ended the run -----------------------------------------

// A killed `br start` / `br boot` must record WHICH signal killed it as the
// cancel cause. Everything downstream can only see "context canceled", which is
// indistinguishable from the Incus readiness budget expiring — and that
// ambiguity is what made a cartridge boot killed from outside look exactly like
// a guest that booted too slowly.
//
// This asserts OUR helper rather than signal.NotifyContext's behavior on
// purpose: the standard library only names the signal on a toolchain newer than
// the one go.mod pins, so a test written against it passes on a developer's
// machine and fails (correctly) in the CI container. See signalContext.
func TestSignalContextRecordsTheSignalAsTheCause(t *testing.T) {
	ctx, stop := signalContext(context.Background(), syscall.SIGUSR1)
	defer stop()

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGUSR1); err != nil {
		t.Fatalf("kill(self, SIGUSR1): %v", err)
	}

	// cancel sets the cause before it closes Done, so waking on Done is enough:
	// there is nothing to poll for and nothing to sleep on.
	select {
	case <-ctx.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("signalContext did not cancel on the signal")
	}

	cause := context.Cause(ctx)
	if !errors.Is(cause, errSignaled) {
		t.Fatalf("context.Cause = %v, want it to wrap %v", cause, errSignaled)
	}
	if errors.Is(cause, context.Canceled) {
		t.Fatalf("context.Cause = %v; an anonymous cancellation cannot name what killed the run", cause)
	}
	if !strings.Contains(cause.Error(), syscall.SIGUSR1.String()) {
		t.Errorf("context.Cause = %q, want it to name %q", cause, syscall.SIGUSR1)
	}
}

// The stop function releases the run the way NotifyContext's does, recording a
// plain cancellation: nobody was signaled, so nothing must claim they were.
func TestSignalContextStopCancelsWithoutASignal(t *testing.T) {
	ctx, stop := signalContext(context.Background(), syscall.SIGUSR2)
	stop()

	select {
	case <-ctx.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("stop() did not cancel the context")
	}
	cause := context.Cause(ctx)
	if !errors.Is(cause, context.Canceled) || errors.Is(cause, errSignaled) {
		t.Errorf("context.Cause = %v, want a plain cancellation", cause)
	}
}

// A parent that is already done is inherited, cause and all: a holder or a test
// that hands in a dead context must not be told a signal arrived.
func TestSignalContextInheritsAParentCause(t *testing.T) {
	parent, cancelParent := context.WithCancelCause(context.Background())
	parentCause := errors.New("parent went away")
	cancelParent(parentCause)

	ctx, stop := signalContext(parent, syscall.SIGUSR1)
	defer stop()

	select {
	case <-ctx.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("a context derived from a canceled parent is not done")
	}
	if cause := context.Cause(ctx); !errors.Is(cause, parentCause) {
		t.Errorf("context.Cause = %v, want the parent's %v", cause, parentCause)
	}
}

// The running summary must not describe an interrupted boot the same way it
// describes one that ran out of budget: the first is already shutting down, so
// "VM is running" and "wait for cloud-init" are both wrong and both send the
// reader after the guest instead of after whoever stopped the boot.
func TestBootSummaryDistinguishesInterruptionFromTimeout(t *testing.T) {
	interrupted := fmt.Errorf("boot interrupted (received signal): %w", context.Canceled)
	timedOut := fmt.Errorf("wait for incus server: budget exhausted: %w", context.DeadlineExceeded)

	if h := bootSummaryHeadline(interrupted); !strings.Contains(h, "interrupted") {
		t.Errorf("bootSummaryHeadline(interrupted) = %q, want it to say the boot was interrupted", h)
	}
	if h := bootSummaryHeadline(interrupted); strings.Contains(h, "VM is running") {
		t.Errorf("bootSummaryHeadline(interrupted) = %q, must not claim the VM is running", h)
	}
	if h := bootSummaryHeadline(timedOut); !strings.Contains(h, "did not finish booting") {
		t.Errorf("bootSummaryHeadline(timedOut) = %q, want the unchanged timeout wording", h)
	}
	if hint := bootSummaryHint(interrupted); strings.Contains(hint, "cloud-init completes") {
		t.Errorf("bootSummaryHint(interrupted) = %q, must not send the user to wait on cloud-init", hint)
	}
	if hint := bootSummaryHint(timedOut); !strings.Contains(hint, "cloud-init completes") {
		t.Errorf("bootSummaryHint(timedOut) = %q, want the unchanged timeout hint", hint)
	}
}
