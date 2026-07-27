package incus

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/lxc/incus/v6/shared/api"
)

// TestCheckAuthorized pins the readiness gate: the Incus API answers GetServer
// even before it has accepted our client cert (Auth=="untrusted"), so readiness
// must distinguish a trusted response from a merely-reachable one. A regression
// here would let a half-started VM whose cert never landed in the trust store be
// reported as ready — the bug this gate fixes.
func TestCheckAuthorized(t *testing.T) {
	tests := []struct {
		name    string
		server  *api.Server
		wantErr bool
	}{
		{
			name:    "trusted is ready",
			server:  &api.Server{ServerUntrusted: api.ServerUntrusted{Auth: authTrusted}},
			wantErr: false,
		},
		{
			name:    "untrusted is not ready",
			server:  &api.Server{ServerUntrusted: api.ServerUntrusted{Auth: "untrusted"}},
			wantErr: true,
		},
		{
			name:    "empty auth is not ready",
			server:  &api.Server{ServerUntrusted: api.ServerUntrusted{Auth: ""}},
			wantErr: true,
		},
		{
			name:    "nil server is not ready",
			server:  nil,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkAuthorized(tc.server)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("checkAuthorized(%+v) = nil, want error", tc.server)
				}
				return
			}
			if err != nil {
				t.Fatalf("checkAuthorized(%+v) = %v, want nil", tc.server, err)
			}
		})
	}
}

// TestCheckAuthorizedErrorSurfacesAuth ensures the untrusted error names the
// observed auth state so the readiness loop's logs point at trust drift rather
// than a generic "not ready".
func TestCheckAuthorizedErrorSurfacesAuth(t *testing.T) {
	err := checkAuthorized(&api.Server{ServerUntrusted: api.ServerUntrusted{Auth: "untrusted"}})
	if err == nil {
		t.Fatal("expected error for untrusted server")
	}
	if !strings.Contains(err.Error(), "untrusted") {
		t.Fatalf("error %q does not surface the auth state", err)
	}
}

// --- how a readiness wait ENDS ---------------------------------------------
//
// A wait that ends has exactly two possible endings and they call for opposite
// responses, so the log and the error must not read the same. "Incus API
// readiness timed out ... err=context canceled" — a timeout that did not happen,
// wrapped around a cancellation that did — is what these tests exist to prevent:
// it sent a real investigation after --timeout when the process had in fact been
// killed from outside.

func TestWaitEndedDistinguishesTimeoutFromCancellation(t *testing.T) {
	probeErr := errors.New("connection reset by peer")

	timedOut, cancelTimedOut := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelTimedOut()
	<-timedOut.Done()

	canceled, cancelCanceled := context.WithCancelCause(context.Background())
	cancelCanceled(errors.New("received signal: terminated"))
	<-canceled.Done()

	tests := []struct {
		name       string
		ctx        context.Context
		wantIs     error
		wantNotIs  error
		wantPhrase []string
	}{
		{
			name:       "budget exhausted",
			ctx:        timedOut,
			wantIs:     context.DeadlineExceeded,
			wantNotIs:  context.Canceled,
			wantPhrase: []string{"budget exhausted", "last probe error: connection reset by peer"},
		},
		{
			name:       "canceled from outside",
			ctx:        canceled,
			wantIs:     context.Canceled,
			wantNotIs:  context.DeadlineExceeded,
			wantPhrase: []string{"canceled after", "received signal: terminated", "last probe error: connection reset by peer"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := waitEnded(tc.ctx, "https://127.0.0.1:18443", 5, 2*time.Minute+26*time.Second, probeErr)
			if err == nil {
				t.Fatal("waitEnded() = nil, want an error")
			}
			if !errors.Is(err, tc.wantIs) {
				t.Errorf("waitEnded() = %v, want it to wrap %v", err, tc.wantIs)
			}
			if errors.Is(err, tc.wantNotIs) {
				t.Errorf("waitEnded() = %v, must NOT read as %v", err, tc.wantNotIs)
			}
			for _, phrase := range tc.wantPhrase {
				if !strings.Contains(err.Error(), phrase) {
					t.Errorf("waitEnded() = %q, want it to mention %q", err, phrase)
				}
			}
		})
	}
}

// A cancellation must never be described as a timeout, in either direction.
// This is the exact confusion that shipped: both endings produced the same
// "timed out" wording.
func TestWaitEndedNeverCallsACancellationATimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	<-ctx.Done()

	err := waitEnded(ctx, "https://127.0.0.1:18443", 5, time.Minute, nil)
	if strings.Contains(err.Error(), "timed out") || strings.Contains(err.Error(), "exhausted") {
		t.Errorf("waitEnded() = %q, want it to say the wait was canceled, not that a budget ran out", err)
	}
	// With no probe having failed there is nothing to append, and the error must
	// not invent a "<nil>" one.
	if strings.Contains(err.Error(), "last probe error") {
		t.Errorf("waitEnded() = %q, want no last-probe clause when no probe failed", err)
	}
}

// The budget in force is logged so a reader of a boot log can tell what
// --timeout was actually resolved to. An unbounded wait says so.
func TestBudgetOf(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if got := budgetOf(ctx); got != "10m0s" {
		t.Errorf("budgetOf(bounded) = %q, want %q", got, "10m0s")
	}
	if got := budgetOf(context.Background()); got != "none" {
		t.Errorf("budgetOf(unbounded) = %q, want %q", got, "none")
	}
}

// WaitForServer itself must carry the distinction out of the loop: an endpoint
// nothing is listening on, with a context that is already done, returns an error
// that names which ending happened. No network beyond a refused loopback dial.
func TestWaitForServerReportsWhyItStopped(t *testing.T) {
	endpoint := "https://" + closedLoopbackAddr(t)

	t.Run("deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
		defer cancel()
		_, err := WaitForServer(ctx, endpoint, nil, nil, time.Millisecond, nil)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("WaitForServer() = %v, want a deadline-exceeded wait error", err)
		}
		if !strings.Contains(err.Error(), "budget exhausted") {
			t.Errorf("WaitForServer() = %q, want it to say the budget ran out", err)
		}
	})

	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := WaitForServer(ctx, endpoint, nil, nil, time.Millisecond, nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("WaitForServer() = %v, want a canceled wait error", err)
		}
		if strings.Contains(err.Error(), "budget exhausted") {
			t.Errorf("WaitForServer() = %q, want it NOT to blame the budget", err)
		}
	})
}

// closedLoopbackAddr returns a loopback host:port that is guaranteed to refuse
// connections: a listener is bound to claim the port and closed immediately.
func closedLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return addr
}
