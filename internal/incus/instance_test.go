package incus_test

import (
	"context"
	"errors"
	"testing"
	"time"

	incusclient "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
	"github.com/stuffbucket/bladerunner/internal/incus"
)

// stallBudget is how long a test waits for a call that a canceled context must
// have already released. It is generous enough that a loaded CI machine does
// not trip it, and short enough that a regression fails the run instead of
// hanging it until the go test deadline.
const stallBudget = 10 * time.Second

// settleDelay is how long a call is left blocked before the test cancels it, so
// that the cancellation lands on a call that is genuinely waiting rather than
// on one that has not started.
const settleDelay = 50 * time.Millisecond

// --- substitutes for the Incus SDK -----------------------------------------

// completedOperation is an exec operation that finishes at once and reports
// exitCode. It stands for the half of a stalled exec that DOES work: the
// operation completes, and only the websocket relay carrying its output is
// stuck. That split is the bug — WaitContext honored the context and the drain
// that followed it did not.
type completedOperation struct {
	incusclient.Operation
	exitCode int
}

func (o *completedOperation) WaitContext(context.Context) error { return nil }

func (o *completedOperation) Get() api.Operation {
	return api.Operation{Metadata: map[string]any{"return": float64(o.exitCode)}}
}

// execServer answers ExecInstance with op. When closeDataDone is false it never
// closes the DataDone channel, which is the stalled relay.
type execServer struct {
	incusclient.InstanceServer
	op            incusclient.Operation
	closeDataDone bool
}

func (s *execServer) ExecInstance(_ string, _ api.InstanceExecPost, args *incusclient.InstanceExecArgs) (incusclient.Operation, error) {
	if s.closeDataDone {
		close(args.DataDone)
	}
	return s.op, nil
}

// listServer answers GetInstancesFull, blocking until release is closed. The
// Incus SDK has no context-aware variant of that call, so a stalled server is
// the only way to show that ListInstances still lets its caller go.
type listServer struct {
	incusclient.InstanceServer
	release   chan struct{}
	instances []api.InstanceFull
}

func (s *listServer) GetInstancesFull(api.InstanceType) ([]api.InstanceFull, error) {
	if s.release != nil {
		<-s.release
	}
	return s.instances, nil
}

// --- the drain that could not be interrupted --------------------------------

// This is issue #283. The exec operation completes, the stdio relay behind it
// stalls, and the caller is left in a bare `<-dataDone` receive that no signal,
// deadline or cancellation can reach. `br exec` hangs with no way out.
//
// The bounded select below is what makes the failure a failure instead of a
// hung CI run: without it, a regression here blocks until the go test deadline
// kills the whole package with no useful message.
func TestExecInstanceReturnsWhenTheStdioDrainStalls(t *testing.T) {
	client := incus.NewClient(&execServer{op: &completedOperation{}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel once the call is genuinely parked in the drain. This is the SIGINT
	// a user sends when `br exec` has stopped responding.
	time.AfterFunc(settleDelay, cancel)

	done := make(chan error, 1)
	go func() {
		_, err := client.ExecInstance(ctx, "box", []string{"sleep", "infinity"}, incus.ExecOptions{})
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ExecInstance() = %v, want it to wrap %v", err, context.Canceled)
		}
	case <-time.After(stallBudget):
		t.Fatal("ExecInstance did not return after its context was canceled: the stdio drain is unbounded")
	}
}

// A deadline is the same escape by a different name, and it is the one a
// non-interactive caller — a script, the menubar, an agent — has. Nobody is
// there to press Ctrl-C for those.
func TestExecInstanceReturnsWhenItsDeadlinePasses(t *testing.T) {
	client := incus.NewClient(&execServer{op: &completedOperation{}})

	ctx, cancel := context.WithTimeout(context.Background(), settleDelay)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := client.ExecInstance(ctx, "box", []string{"sleep", "infinity"}, incus.ExecOptions{})
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("ExecInstance() = %v, want it to wrap %v", err, context.DeadlineExceeded)
		}
	case <-time.After(stallBudget):
		t.Fatal("ExecInstance ignored its deadline: the stdio drain is unbounded")
	}
}

// The escape must not cost the normal path. A relay that finishes still yields
// the remote exit code, and the context is never consulted for it.
func TestExecInstanceReturnsTheRemoteExitCode(t *testing.T) {
	const wantCode = 42
	client := incus.NewClient(&execServer{
		op:            &completedOperation{exitCode: wantCode},
		closeDataDone: true,
	})

	code, err := client.ExecInstance(context.Background(), "box", []string{"false"}, incus.ExecOptions{})
	if err != nil {
		t.Fatalf("ExecInstance() error = %v, want nil", err)
	}
	if code != wantCode {
		t.Errorf("ExecInstance() = %d, want %d", code, wantCode)
	}
}

// The guards in front of the call, reached from outside the package.
func TestExecInstanceRefusesWhatItCannotRun(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := (&incus.Client{}).ExecInstance(canceled, "box", []string{"true"}, incus.ExecOptions{}); !errors.Is(err, context.Canceled) {
		t.Errorf("ExecInstance(canceled ctx) = %v, want it to wrap %v", err, context.Canceled)
	}
	if _, err := (&incus.Client{}).ExecInstance(context.Background(), "box", nil, incus.ExecOptions{}); err == nil {
		t.Error("ExecInstance(no command) = nil, want an error")
	}
}

// --- the list that could not be interrupted ---------------------------------

// `br ls` and shell completion both reach an Incus API that can stop answering.
// ListInstances documented that it only looked at the context before it issued
// the request, which made a cancellation or a deadline on that context a
// promise it did not keep.
func TestListInstancesReturnsWhenTheRequestStalls(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	client := incus.NewClient(&listServer{release: release})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(settleDelay, cancel)

	done := make(chan error, 1)
	go func() {
		_, err := client.ListInstances(ctx)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ListInstances() = %v, want it to wrap %v", err, context.Canceled)
		}
	case <-time.After(stallBudget):
		t.Fatal("ListInstances did not return after its context was canceled: the request is unbounded")
	}
}

// A server that answers is still passed straight through.
func TestListInstancesReturnsTheInstances(t *testing.T) {
	client := incus.NewClient(&listServer{
		instances: []api.InstanceFull{{Instance: api.Instance{Name: "box"}}},
	})

	instances, err := client.ListInstances(context.Background())
	if err != nil {
		t.Fatalf("ListInstances() error = %v, want nil", err)
	}
	if len(instances) != 1 || instances[0].Name != "box" {
		t.Errorf("ListInstances() = %+v, want one instance named box", instances)
	}
}

// NewClient is the seam Connect itself uses; a client built through it answers
// the same calls.
func TestNewClientWrapsAServer(t *testing.T) {
	if client := incus.NewClient(&listServer{}); client == nil {
		t.Fatal("NewClient() = nil, want a client")
	}
}
