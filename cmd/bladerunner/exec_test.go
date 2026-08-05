package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stuffbucket/bladerunner/internal/incus"
	"golang.org/x/term"
)

// interruptBudget bounds a wait on a cancellation that must already have
// happened, so a regression fails the run rather than hanging it.
const interruptBudget = 30 * time.Second

// A foreground command must run its work under a context that can END. This is
// the half of issue #283 that lived in package main: `br exec` and `br ls`
// passed context.Background(), whose Done channel is nil, so every cancellation
// check downstream of them was dead code no matter how correct it was.
//
// The nil Done channel is the whole tell, and it is checked before anything
// below raises a signal: a context that cannot be canceled is also a context in
// which raising SIGINT kills this test binary outright.
func TestInterruptibleContextCanBeCanceled(t *testing.T) {
	ctx, stop := interruptibleContext()
	defer stop()

	if ctx.Done() == nil {
		t.Fatal("interruptibleContext() has a nil Done channel: context.Background() can never be interrupted")
	}
}

// Ctrl-C on a non-interactive `br exec` must cancel the operation. Raising the
// real signal is the only way to hold that claim — a handler that was never
// registered would let SIGINT terminate this process, which is precisely the
// old behavior and precisely what the acceptance criterion rejects.
func TestInterruptibleContextCancelsOnInterrupt(t *testing.T) {
	ctx, stop := interruptibleContext()
	defer stop()

	// Guard first. Signaling a process whose handler is missing kills it.
	if ctx.Done() == nil {
		t.Fatal("interruptibleContext() has a nil Done channel; refusing to raise SIGINT")
	}

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("kill(self, SIGINT): %v", err)
	}

	select {
	case <-ctx.Done():
	case <-time.After(interruptBudget):
		t.Fatal("interruptibleContext() did not cancel on SIGINT")
	}

	// The cause names the signal, so a canceled exec can be told apart from one
	// that hit a deadline of its own.
	cause := context.Cause(ctx)
	if !errors.Is(cause, errSignaled) {
		t.Fatalf("context.Cause = %v, want it to wrap %v", cause, errSignaled)
	}
	if !strings.Contains(cause.Error(), syscall.SIGINT.String()) {
		t.Errorf("context.Cause = %q, want it to name %q", cause, syscall.SIGINT)
	}
}

// SIGTERM is the interrupt a script or a supervisor sends, and it is the one
// that used to leave a raw terminal behind: the default disposition killed the
// process before `br exec` could run its deferred restore. It must cancel too.
func TestInterruptibleContextCancelsOnTerminate(t *testing.T) {
	ctx, stop := interruptibleContext()
	defer stop()

	if ctx.Done() == nil {
		t.Fatal("interruptibleContext() has a nil Done channel; refusing to raise SIGTERM")
	}

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("kill(self, SIGTERM): %v", err)
	}

	select {
	case <-ctx.Done():
	case <-time.After(interruptBudget):
		t.Fatal("interruptibleContext() did not cancel on SIGTERM")
	}
}

// Both signals are honored, and nothing else has been added to the set: a
// command that swallowed more than it cancels on would take away escapes the
// user still needs.
func TestInterruptSignals(t *testing.T) {
	want := map[os.Signal]bool{syscall.SIGINT: false, syscall.SIGTERM: false}
	for _, sig := range interruptSignals {
		seen, ok := want[sig]
		if !ok {
			t.Errorf("interruptSignals includes %v, which no command acts on", sig)
			continue
		}
		if seen {
			t.Errorf("interruptSignals lists %v twice", sig)
		}
		want[sig] = true
	}
	for sig, seen := range want {
		if !seen {
			t.Errorf("interruptSignals omits %v", sig)
		}
	}
}

// stop() releases the registration without claiming a signal arrived, so a
// command that finished normally does not report itself as interrupted.
func TestInterruptibleContextStopIsNotASignal(t *testing.T) {
	ctx, stop := interruptibleContext()
	stop()

	select {
	case <-ctx.Done():
	case <-time.After(interruptBudget):
		t.Fatal("stop() did not cancel the context")
	}
	if cause := context.Cause(ctx); errors.Is(cause, errSignaled) {
		t.Errorf("context.Cause = %v, want a plain cancellation", cause)
	}
}

// Shell completion cannot be interrupted from the keyboard — the shell holds it
// — so its bound has to be a deadline the process sets itself. A zero budget
// would restore the unbounded call that hangs a user's shell.
func TestCompletionBudgetIsBounded(t *testing.T) {
	if completionBudget <= 0 {
		t.Fatalf("completionBudget = %v; shell completion would block the user's shell forever", completionBudget)
	}
}

// configureTTY without --tty must leave the terminal alone and hand back a
// restore that is safe to defer on every exit path, including the canceled one.
func TestConfigureTTYRestoreIsAlwaysSafe(t *testing.T) {
	t.Cleanup(func() { execFlags.tty = false })

	execFlags.tty = false
	opts := incus.ExecOptions{}
	restore := configureTTY(&opts)
	if restore == nil {
		t.Fatal("configureTTY() = nil, want a restore function to defer")
	}
	restore()
	if opts.Width != 0 || opts.Height != 0 {
		t.Errorf("configureTTY() set size %dx%d without --tty", opts.Width, opts.Height)
	}

	// With --tty over a stdin that is not a terminal there is no raw mode to
	// enter and none to leave. Skipped when go test was handed a real terminal:
	// putting the developer's own terminal into raw mode is not this test's
	// business, and a failure in between would leave it that way.
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return
	}
	execFlags.tty = true
	opts = incus.ExecOptions{}
	restore = configureTTY(&opts)
	if restore == nil {
		t.Fatal("configureTTY() = nil for a non-terminal stdin, want a restore function")
	}
	restore()
}
