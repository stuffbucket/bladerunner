package e2e

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// bgProcess owns the one and only Wait on a launched child process.
//
// The boot test used to have two owners: the readiness poller started a
// goroutine on Cmd.Wait, and teardown later started a second one on
// Process.Wait. Go releases a process exactly once, so on every successful boot
// the two raced — the loser reported a bogus error or blocked until a fallback
// timer fired, and that fallback killed the child without reaping it at all
// (#231).
//
// Here the reap happens in one goroutine, created by startBackground. Every
// other observer reads the outcome through done or wait, so a second reap is
// not expressible rather than merely unlikely: nothing outside this file holds
// the *exec.Cmd.
type bgProcess struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	reaped chan struct{} // closed by the owner goroutine once the child is reaped
	err    error         // written by the owner BEFORE reaped is closed
}

// startBackground starts cmd and hands its reap to a single owner goroutine.
// cancel must be the CancelFunc of the context cmd was built with; stop calls
// it. The returned process is nil when the launch itself failed, and every
// method tolerates a nil receiver so teardown stays a no-op in that case.
func startBackground(cmd *exec.Cmd, cancel context.CancelFunc) (*bgProcess, error) {
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", cmd.Path, err)
	}
	p := &bgProcess{cmd: cmd, cancel: cancel, reaped: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		close(p.reaped)
	}()
	return p, nil
}

// done returns a channel closed once the child has been reaped. A nil process
// returns a nil channel, which blocks forever — the correct reading of "this
// process will never exit because it never started".
func (p *bgProcess) done() <-chan struct{} {
	if p == nil {
		return nil
	}
	return p.reaped
}

// wait blocks until the child has been reaped and returns the reap result. Any
// number of callers may call it any number of times: the reap itself happened
// once, in the owner goroutine, and the channel close orders that write before
// every read here.
func (p *bgProcess) wait() error {
	if p == nil {
		return nil
	}
	<-p.reaped
	return p.err
}

// stop ends the child and does not return until it is positively reaped. It
// cancels first, gives the child grace to exit on its own, then sends SIGKILL —
// which cannot be caught, so the final wait always terminates. Blocking on that
// wait is the point: teardown must never return while the child is unreaped.
//
// Calling stop on a nil process, or more than once, is harmless.
func (p *bgProcess) stop(grace time.Duration) error {
	if p == nil {
		return nil
	}
	p.cancel()
	select {
	case <-p.reaped:
		return p.err
	case <-time.After(grace):
	}
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	return p.wait()
}

// --- tests -----------------------------------------------------------------

const (
	// testGrace is the stop grace used where the child is expected to go
	// quietly; testWedgedGrace is the short grace used to force the escalation
	// path deliberately.
	testGrace       = 10 * time.Second
	testWedgedGrace = 200 * time.Millisecond

	// testSettle bounds how long a goroutine count is given to return to base.
	testSettle     = 5 * time.Second
	testSettlePoll = 10 * time.Millisecond

	// wedgedExit is the status a subject shell exits with when a test wants a
	// specific, recognizable failure.
	wedgedExit = 7
)

// longSleepArgs is a command that outlives any test unless it is stopped.
func longSleepArgs(t *testing.T) []string {
	t.Helper()
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skipf("sleep not found in PATH: %v", err)
	}
	return []string{"sleep", "300"}
}

// newBackground starts a child under a single owner and registers a stop so a
// failing assertion cannot strand it.
func newBackground(t *testing.T, args ...string) *bgProcess {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	p, err := startBackground(cmd, cancel)
	if err != nil {
		cancel()
		t.Fatalf("startBackground(%v): %v", args, err)
	}
	t.Cleanup(func() { _ = p.stop(testGrace) })
	return p
}

// assertReapedOnce fails unless os/exec reports that Wait has already been
// called. That is the decisive check: it can only be true if the owner
// goroutine reaped the child, and it stays true no matter how many observers
// asked for the result.
func assertReapedOnce(t *testing.T, p *bgProcess) {
	t.Helper()
	err := p.cmd.Wait()
	if err == nil || !strings.Contains(err.Error(), "Wait was already called") {
		t.Fatalf("the child was not reaped by its owner; a second Wait returned %v", err)
	}
}

// assertSettled fails unless the goroutine count has returned to base, which is
// how a goroutine still blocked on a reaped child shows up.
func assertSettled(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(testSettle)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= base {
			return
		}
		time.Sleep(testSettlePoll)
	}
	t.Fatalf("goroutines did not settle: %d now, %d at the start", runtime.NumGoroutine(), base)
}

func TestBgProcessStopReapsExactlyOnce(t *testing.T) {
	base := runtime.NumGoroutine()
	p := newBackground(t, longSleepArgs(t)...)

	start := time.Now()
	first := p.stop(testGrace)
	if elapsed := time.Since(start); elapsed >= testGrace {
		t.Fatalf("stop took %s; cancellation should have ended the child well inside the %s grace", elapsed, testGrace)
	}
	if first == nil {
		t.Fatal("stop reported no error for a canceled child; it should report the signal")
	}
	assertReapedOnce(t, p)

	// Idempotent: a second stop returns the same recorded result and does not
	// block, reap again, or panic.
	if second := p.stop(testGrace); !errors.Is(second, first) && second.Error() != first.Error() {
		t.Fatalf("second stop returned %v, want the recorded %v", second, first)
	}
	if third := p.wait(); third.Error() != first.Error() {
		t.Fatalf("wait returned %v, want the recorded %v", third, first)
	}
	assertSettled(t, base)
}

func TestBgProcessStopKillsAndReapsAWedgedChild(t *testing.T) {
	base := runtime.NumGoroutine()
	args := longSleepArgs(t)
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	// A child that ignores cancellation. exec.CommandContext would otherwise
	// kill it for us, and the escalation path would never be exercised.
	cmd.Cancel = func() error { return nil }
	p, err := startBackground(cmd, cancel)
	if err != nil {
		cancel()
		t.Fatalf("startBackground: %v", err)
	}

	start := time.Now()
	if serr := p.stop(testWedgedGrace); serr == nil {
		t.Fatal("stop reported no error for a killed child")
	}
	elapsed := time.Since(start)
	if elapsed > testGrace {
		t.Fatalf("stop took %s on a wedged child; it must escalate after the %s grace", elapsed, testWedgedGrace)
	}
	// stop returned, so the child is already reaped — not merely signaled.
	select {
	case <-p.done():
	default:
		t.Fatal("stop returned before the wedged child was reaped")
	}
	assertReapedOnce(t, p)
	assertSettled(t, base)
}

func TestBgProcessReportsEarlyExit(t *testing.T) {
	base := runtime.NumGoroutine()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("sh not found in PATH: %v", err)
	}
	p := newBackground(t, "sh", "-c", fmt.Sprintf("exit %d", wedgedExit))

	err := p.wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("wait returned %v, want an *exec.ExitError", err)
	}
	if exit.ExitCode() != wedgedExit {
		t.Fatalf("exit code %d, want %d", exit.ExitCode(), wedgedExit)
	}

	// Teardown after an early exit must be instant and must report the same
	// error, not a second (failing) reap.
	start := time.Now()
	if serr := p.stop(testGrace); serr.Error() != err.Error() {
		t.Fatalf("stop returned %v, want the recorded %v", serr, err)
	}
	if elapsed := time.Since(start); elapsed >= testGrace {
		t.Fatalf("stop waited %s for an already-exited child", elapsed)
	}
	assertReapedOnce(t, p)
	assertSettled(t, base)
}

func TestBgProcessNilIsInert(t *testing.T) {
	var p *bgProcess
	if err := p.stop(testGrace); err != nil {
		t.Fatalf("stop on a nil process returned %v", err)
	}
	if err := p.wait(); err != nil {
		t.Fatalf("wait on a nil process returned %v", err)
	}
	select {
	case <-p.done():
		t.Fatal("done on a nil process is ready; it must block, because that process never exits")
	default:
	}
}

func TestStartBackgroundReportsLaunchFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "/nonexistent/br", "start")
	p, err := startBackground(cmd, cancel)
	if err == nil {
		_ = p.stop(testGrace)
		t.Fatal("startBackground reported no error for a binary that does not exist")
	}
	if p != nil {
		t.Fatalf("startBackground returned a process (%v) alongside an error", p)
	}
}
