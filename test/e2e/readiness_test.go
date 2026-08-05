package e2e

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These tests pin the lifecycle contract of #231 without a VM: one waiter, one
// deadline. They drive the same readiness loop and the same teardown the gated
// boot smoke test drives, with the control-plane probes replaced by functions
// that consume exactly the budget they are handed. Run them with -race.

const (
	// fakePoll is the retry gap in these tests; small enough that a run takes
	// milliseconds, large enough not to spin.
	fakePoll = 5 * time.Millisecond

	// fakeCeiling is the per-probe ceiling. It is deliberately far LARGER than
	// any deadline used below: the whole point is that the clamp, not the
	// ceiling, decides how long a probe may run. Before #231 this was the 90s
	// cmdTimeout and a stalled control plane overran the budget by minutes.
	fakeCeiling = 30 * time.Second

	// fakeDeadline is the overall budget the loop must respect, and
	// fakeSlack is the scheduling margin allowed on top of it.
	fakeDeadline = 300 * time.Millisecond
	fakeSlack    = 2 * time.Second
)

// stalledProbe consumes its entire budget and then fails, which is how a
// wedged control plane behaves. If the budget were not clamped to the remaining
// deadline, one call would block for fakeCeiling.
func stalledProbe(calls *int) probeFunc {
	return func(budget time.Duration) (string, error) {
		*calls++
		time.Sleep(budget)
		return "", errors.New("control plane not answering")
	}
}

// exitedProcess returns a bgProcess whose child has already been reaped, so the
// readiness loop's early-exit branch is deterministic rather than a race.
func exitedProcess(t *testing.T, code int) *bgProcess {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("sh not found in PATH: %v", err)
	}
	p := newBackground(t, "sh", "-c", "exit "+strconv.Itoa(code))
	_ = p.wait()
	return p
}

func TestReadinessReturnsOnFirstValidJSON(t *testing.T) {
	base := runtime.NumGoroutine()
	proc := newBackground(t, longSleepArgs(t)...)
	statusCalls := 0
	r := readiness{
		ls:     func(time.Duration) (string, error) { return `{"instances":[]}`, nil },
		status: func(time.Duration) (string, error) { statusCalls++; return `{"status":"running"}`, nil },
		poll:   fakePoll,
		budget: fakeCeiling,
		proc:   proc,
		log:    func() string { return "" },
	}

	out, err := r.wait(t, fakeDeadline)
	if err != nil {
		t.Fatalf("wait returned %v, want success", err)
	}
	if !strings.Contains(out, "instances") {
		t.Fatalf("wait returned %q, want the probe output", out)
	}
	if statusCalls != 0 {
		t.Fatalf("the diagnostic status probe ran %d times on the success path", statusCalls)
	}

	// The success path must leave the process running and unreaped: readiness
	// does not own it. Teardown then completes without the escalation.
	select {
	case <-proc.done():
		t.Fatal("readiness reaped the process; teardown owns that")
	default:
	}
	start := time.Now()
	teardown(t, trueBinary(t), os.Environ(), proc, reapGrace)
	if elapsed := time.Since(start); elapsed >= reapGrace {
		t.Fatalf("teardown took %s; it hit the %s fallback on a healthy process", elapsed, reapGrace)
	}
	assertReapedOnce(t, proc)
	assertSettled(t, base)
}

func TestReadinessStaysWithinTheDeadline(t *testing.T) {
	base := runtime.NumGoroutine()
	proc := newBackground(t, longSleepArgs(t)...)
	lsCalls, statusCalls := 0, 0
	r := readiness{
		ls:     stalledProbe(&lsCalls),
		status: stalledProbe(&statusCalls),
		poll:   fakePoll,
		budget: fakeCeiling,
		proc:   proc,
		log:    func() string { return "" },
	}

	start := time.Now()
	if _, err := r.wait(t, fakeDeadline); err == nil {
		t.Fatal("wait succeeded against a control plane that never answers")
	}
	elapsed := time.Since(start)
	if elapsed > fakeDeadline+fakeSlack {
		t.Fatalf("readiness took %s against a %s deadline; probes are not clamped to the "+
			"remaining budget (each would run for the %s ceiling)", elapsed, fakeDeadline, fakeCeiling)
	}
	if lsCalls == 0 {
		t.Fatal("the readiness probe never ran")
	}
	// readiness never touched the process; its single owner still ends it.
	_ = proc.stop(testGrace)
	assertReapedOnce(t, proc)
	assertSettled(t, base)
}

func TestReadinessReportsAnEarlyProcessExit(t *testing.T) {
	base := runtime.NumGoroutine()
	proc := exitedProcess(t, wedgedExit)
	calls := 0
	r := readiness{
		ls:     func(time.Duration) (string, error) { calls++; return "", errors.New("not up") },
		status: func(time.Duration) (string, error) { return "", errors.New("not up") },
		poll:   time.Hour, // the ticker must not be what ends this wait
		budget: fakeCeiling,
		proc:   proc,
		log:    func() string { return "br start said something" },
	}

	_, err := r.wait(t, fakeCeiling)
	var early *earlyExitError
	if !errors.As(err, &early) {
		t.Fatalf("wait returned %v, want an *earlyExitError", err)
	}
	if !strings.Contains(early.Error(), "br start said something") {
		t.Fatalf("the early-exit error dropped the process log: %v", early)
	}
	var exit *exec.ExitError
	if !errors.As(early.err, &exit) || exit.ExitCode() != wedgedExit {
		t.Fatalf("the early-exit error reported %v, want exit status %d", early.err, wedgedExit)
	}

	// The reap already happened in the owner goroutine, so teardown is instant
	// and nothing waits a second time.
	start := time.Now()
	teardown(t, trueBinary(t), os.Environ(), proc, reapGrace)
	if elapsed := time.Since(start); elapsed >= reapGrace {
		t.Fatalf("teardown took %s for an already-exited process", elapsed)
	}
	assertReapedOnce(t, proc)
	assertSettled(t, base)
}

func TestTeardownIsInertWithoutALaunch(t *testing.T) {
	base := runtime.NumGoroutine()
	// A launch that failed leaves proc nil. Teardown must still run br stop and
	// must not panic or block on a process that does not exist.
	teardown(t, trueBinary(t), os.Environ(), nil, reapGrace)
	assertSettled(t, base)
}

func TestTeardownKillsAndReapsAWedgedProcess(t *testing.T) {
	base := runtime.NumGoroutine()
	args := longSleepArgs(t)
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Cancel = func() error { return nil } // ignores cancellation, like a wedged server
	proc, err := startBackground(cmd, cancel)
	if err != nil {
		cancel()
		t.Fatalf("startBackground: %v", err)
	}

	// A short grace forces the escalation path deliberately; the assertion is
	// that teardown still returns only after the kill has been REAPED, which is
	// what the old 30-second fallback skipped.
	start := time.Now()
	teardown(t, trueBinary(t), os.Environ(), proc, testWedgedGrace)
	if elapsed := time.Since(start); elapsed > testGrace {
		t.Fatalf("teardown took %s to escalate past a %s grace", elapsed, testWedgedGrace)
	}

	// Teardown returned, so the kill has already been confirmed by the owner.
	select {
	case <-proc.done():
	default:
		t.Fatal("teardown returned while the wedged process was still unreaped")
	}
	assertReapedOnce(t, proc)
	assertSettled(t, base)
}

// trueBinary is a harmless stand-in for the `br` binary: teardown runs
// `<bin> stop --force`, and `true` accepts any arguments and exits 0.
func trueBinary(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("true")
	if err != nil {
		t.Skipf("true not found in PATH: %v", err)
	}
	return bin
}
