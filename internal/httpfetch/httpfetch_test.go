package httpfetch_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stuffbucket/bladerunner/internal/httpfetch"
)

// testStall is the stall budget every test here uses. It is short enough that
// a regression reports in milliseconds and long enough that a loaded CI box
// does not trip it by accident.
const testStall = 250 * time.Millisecond

// testDeadline bounds every wait in this file. A stall watchdog that does not
// fire is a HANG, and a hanging test is indistinguishable from a slow one
// until the whole job times out, so every assertion is made against a clock.
const testDeadline = 15 * time.Second

// stallingServer serves /stall: it sends a 200, one byte, a flush, and then
// nothing at all while holding the connection open. That is the exact shape of
// the failure in #282 — TLS completed, headers delivered, progress bar sitting
// at 1 byte forever.
//
// The handler unblocks when the client gives up (the request context is
// canceled) or when the test releases it, so nothing here can outlive the
// test that started it.
func stallingServer(t *testing.T) (*httptest.Server, func()) {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("x"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	// Ordering matters: the handler must be released before Close, because
	// httptest.Server.Close waits for outstanding requests.
	return srv, func() {
		close(release)
		srv.Close()
	}
}

// readAll drains body in a goroutine and reports the outcome, or fails the
// test if nothing comes back inside testDeadline.
func readAll(t *testing.T, body io.Reader) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, body)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(testDeadline):
		t.Fatalf("the read never returned; the transfer is hung, which is the defect")
		return nil
	}
}

// A stalled body must fail, and it must say it stalled. The underlying failure
// is a context cancellation, which would read like the user pressed Ctrl-C.
func TestGetAbandonsABodyThatStopsArriving(t *testing.T) {
	srv, stop := stallingServer(t)
	defer stop()

	start := time.Now()
	resp, err := httpfetch.Get(t.Context(), httpfetch.StreamingClient(), srv.URL+"/stall", testStall)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	err = readAll(t, resp.Body)
	if err == nil {
		t.Fatal("a body that stopped arriving was read to completion")
	}
	if !errors.Is(err, httpfetch.ErrStalled) {
		t.Fatalf("error %v does not wrap ErrStalled; a stall must not be reported as a cancellation", err)
	}
	if elapsed := time.Since(start); elapsed > testDeadline/2 {
		t.Errorf("the stall took %s to report, far past the %s budget", elapsed, testStall)
	}
}

// The budget is on time WITHOUT PROGRESS, not on total duration. A transfer
// that keeps delivering must survive a total time well past the budget —
// that is the whole reason a ~1 GB image cannot use a flat timeout.
func TestGetAllowsASlowButProgressingTransfer(t *testing.T) {
	const chunks = 6
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for range chunks {
			_, _ = w.Write([]byte("chunk"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(testStall / 2)
		}
	}))
	defer srv.Close()

	resp, err := httpfetch.Get(t.Context(), httpfetch.StreamingClient(), srv.URL, testStall)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("a transfer that never stopped progressing was abandoned: %v", err)
	}
	if want := chunks * len("chunk"); len(body) != want {
		t.Errorf("read %d bytes, want %d", len(body), want)
	}
}

// A zero budget installs no watchdog, for a caller whose client already
// carries a flat deadline.
func TestGetWithoutABudgetInstallsNoWatchdog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(testStall * 2)
		_, _ = w.Write([]byte("late but complete"))
	}))
	defer srv.Close()

	resp, err := httpfetch.Get(t.Context(), httpfetch.StreamingClient(), srv.URL, 0)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(body) != "late but complete" {
		t.Errorf("body = %q", string(body))
	}
}

// Client's deadline covers the whole exchange, including a peer that accepts
// the connection and then never produces a status line. This is the sidecar
// case: small, fast, and fatal to a cold boot when it hangs.
func TestClientAppliesAFlatDeadline(t *testing.T) {
	srv, stop := silentServer(t)
	defer stop()

	done := make(chan error, 1)
	go func() {
		resp, err := httpfetch.Get(t.Context(), httpfetch.Client(testStall), srv.URL, 0)
		if err == nil {
			_ = resp.Body.Close()
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a server that never answered was waited on to completion")
		}
	case <-time.After(testDeadline):
		t.Fatal("the flat deadline never fired")
	}
}

// silentServer accepts the request and answers nothing at all.
func silentServer(t *testing.T) (*httptest.Server, func()) {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	return srv, func() {
		close(release)
		srv.Close()
	}
}

// StreamingClient must carry NO flat deadline: a ~1 GB image over a slow link
// is a legitimate transfer that a total-duration cap would kill.
func TestStreamingClientCarriesNoFlatDeadline(t *testing.T) {
	if got := httpfetch.StreamingClient().Timeout; got != 0 {
		t.Errorf("StreamingClient().Timeout = %s, want 0", got)
	}
	if got := httpfetch.Client(testStall).Timeout; got != testStall {
		t.Errorf("Client(%s).Timeout = %s", testStall, got)
	}
}

// Both constructors must set ResponseHeaderTimeout. Waiting for the first
// header byte is never a legitimately slow transfer, so it is bounded whatever
// the body's shape.
func TestEveryClientBoundsTheHeaderWait(t *testing.T) {
	for name, client := range map[string]*http.Client{
		"Client":          httpfetch.Client(testStall),
		"StreamingClient": httpfetch.StreamingClient(),
	} {
		transport, ok := client.Transport.(*http.Transport)
		if !ok {
			t.Errorf("%s: transport is %T, not *http.Transport", name, client.Transport)
			continue
		}
		if transport.ResponseHeaderTimeout != httpfetch.ResponseHeaderTimeout {
			t.Errorf("%s: ResponseHeaderTimeout = %s, want %s",
				name, transport.ResponseHeaderTimeout, httpfetch.ResponseHeaderTimeout)
		}
	}
}

// Closing the body must stop the watchdog, so a caller that abandons a
// response early leaves no timer behind that could fire against a reused
// connection.
func TestClosingTheBodyStopsTheWatchdog(t *testing.T) {
	srv, stop := stallingServer(t)
	defer stop()

	resp, err := httpfetch.Get(t.Context(), httpfetch.StreamingClient(), srv.URL+"/stall", testStall)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	time.Sleep(testStall * 2)

	// A second close must still be safe; nothing above should have panicked.
	_ = resp.Body.Close()
}
