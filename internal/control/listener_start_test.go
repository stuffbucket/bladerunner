package control_test

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stuffbucket/bladerunner/internal/control"
)

// stubAddr is the address a stubListener reports. Nothing reads it; net.Listener
// requires the method.
type stubAddr struct{}

func (stubAddr) Network() string { return "unix" }
func (stubAddr) String() string  { return "stub" }

// stubListener is a net.Listener whose Accept always fails, counting the calls.
// The count is what makes a spinning accept loop measurable instead of merely
// suspected.
//
// limit/onLimit are the guard rail: an accept loop that never returns would
// otherwise run until the test binary is killed, so the listener cancels the
// context once it has been asked often enough to prove the defect. The test
// then finishes and reports the count.
type stubListener struct {
	mu      sync.Mutex
	calls   int
	err     error
	limit   int
	onLimit func()
}

func (s *stubListener) Accept() (net.Conn, error) {
	s.mu.Lock()
	s.calls++
	reached := s.calls == s.limit
	err := s.err
	s.mu.Unlock()
	if reached && s.onLimit != nil {
		s.onLimit()
	}
	return nil, err
}

func (s *stubListener) Close() error   { return nil }
func (s *stubListener) Addr() net.Addr { return stubAddr{} }

func (s *stubListener) acceptCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// stubTransport hands a Listener the stub net.Listener above. Dial has to fail:
// bindListener treats a successful dial as "already running".
type stubTransport struct{ ln net.Listener }

func (t stubTransport) Listen(string) (net.Listener, error) { return t.ln, nil }
func (t stubTransport) Dial(string, time.Duration) (net.Conn, error) {
	return nil, errors.New("nothing is listening")
}
func (t stubTransport) Cleanup(string) error { return nil }

// startWithStub runs Start against a stub listener and returns once Start has
// returned, or fails the test if it never does.
func startWithStub(ctx context.Context, t *testing.T, ln *stubListener) {
	t.Helper()
	l, err := control.NewListenerWithConfig(control.ListenerConfig{
		StateDir:  t.TempDir(),
		Transport: stubTransport{ln: ln},
	})
	if err != nil {
		t.Fatalf("NewListenerWithConfig: %v", err)
	}
	defer func() { _ = l.Close() }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Start(ctx)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Start never returned")
	}
}

// TestStartReturnsOnClosedListener holds the contract that a closed listener
// ends the accept loop.
//
// Teardown closes the control listener while the context is still live (the
// holder cancels its context after the step stack unwinds, not before), so
// "closed listener, live context" is a state the loop really reaches. Accept
// then returns net.ErrClosed immediately and permanently: continuing burns a
// core and writes one log line per iteration for the rest of teardown, which
// rotates away the log history explaining why the VM went down.
func TestStartReturnsOnClosedListener(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The listener is closed, so exactly one Accept is the correct count: the
	// one that discovers it. Any more is the spin.
	const spinEvidence = 500
	ln := &stubListener{err: net.ErrClosed, limit: spinEvidence, onLimit: cancel}

	startWithStub(ctx, t, ln)

	if got := ln.acceptCalls(); got != 1 {
		t.Fatalf("Accept called %d times on a closed listener, want 1: the loop spins instead of returning", got)
	}
}

// TestStartBacksOffOnTransientAcceptError holds the contract that an accept
// error the loop does not recognize is retried with a pause rather than
// immediately. Without the pause an error that persists (EMFILE, say) is the
// same hot loop as a closed listener.
func TestStartBacksOffOnTransientAcceptError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	// With backoff, 300ms allows roughly six retries (5+10+20+40+80+160ms).
	// Without it the loop reaches this limit in microseconds.
	const spinEvidence = 200
	ln := &stubListener{err: errors.New("accept: too many open files"), limit: spinEvidence, onLimit: cancel}

	startWithStub(ctx, t, ln)

	const maxRetries = 12
	if got := ln.acceptCalls(); got > maxRetries {
		t.Fatalf("Accept called %d times in 300ms, want at most %d: the retry has no backoff", got, maxRetries)
	}
}
