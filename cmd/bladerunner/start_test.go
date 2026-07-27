package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

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
