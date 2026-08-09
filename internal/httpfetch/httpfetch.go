// Package httpfetch owns outbound HTTP fetching: how long bladerunner waits
// for a peer, and how it decides that a transfer has stopped.
//
// It exists because "how long do we wait" was answered separately at every
// call site, and three of them answered it with http.DefaultClient — which
// sets no Timeout at all. http.DefaultTransport bounds the TLS handshake and
// nothing after it, so a peer that completes the handshake, sends headers and
// then trickles one byte an hour holds a boot open forever (#282).
//
// Two shapes need two different bounds, and the difference is the reason this
// package has two constructors:
//
//   - A SMALL body (a checksum sidecar, a release manifest) is bounded by a
//     flat client Timeout. It covers the whole exchange, and anything slower
//     than the budget is broken rather than slow.
//   - A LARGE body (a ~1 GB guest image) must NOT carry a flat timeout. A flat
//     timeout caps total transfer time, so it fails an honest download over a
//     slow link while still permitting a peer to trickle bytes indefinitely
//     underneath it. What such a transfer needs bounded is time WITHOUT
//     PROGRESS, which is what Get's stall watchdog measures.
//
// ResponseHeaderTimeout is set for both, because waiting for the first header
// byte is never a legitimately slow transfer.
package httpfetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// ResponseHeaderTimeout bounds the wait between a request going out and its
// response headers coming back. It applies to every client this package
// builds. A peer that has accepted the connection but produced no status line
// within this window is not slow, it is wedged.
const ResponseHeaderTimeout = 30 * time.Second

// StallTimeout is the default budget for a streaming transfer to make no
// progress at all. It is deliberately long enough to survive a stalled TCP
// window or a mirror pausing under load, and short enough that a wedged peer
// surfaces as an error instead of an unkillable boot.
const StallTimeout = 60 * time.Second

// ErrStalled reports that a transfer received no data within its stall budget.
// It is distinct from a timeout on total duration: a stalled transfer had a
// live connection and simply stopped delivering bytes.
var ErrStalled = errors.New("transfer stalled")

// sharedTransport is the one transport every client here uses, so connections
// and their TLS sessions are pooled across fetches rather than rebuilt per
// call.
var sharedTransport = newTransport()

// newTransport clones the standard transport and adds the header bound it
// omits. Cloning rather than building one from scratch keeps proxy support,
// HTTP/2 and the standard dial and idle-connection settings.
func newTransport() *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{ResponseHeaderTimeout: ResponseHeaderTimeout}
	}
	t := base.Clone()
	t.ResponseHeaderTimeout = ResponseHeaderTimeout
	return t
}

// Client returns a client with a flat overall deadline covering connection,
// headers and body. Use it for a body small enough that total duration is a
// meaningful bound — a checksum sidecar, a manifest, a release descriptor.
//
// Do not use it for a multi-hundred-megabyte body. See StreamingClient.
func Client(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: sharedTransport}
}

// StreamingClient returns a client with NO overall deadline, for a body whose
// honest transfer time cannot be predicted. Its liveness bound comes from the
// stall watchdog in Get instead, so a legitimately slow link finishes while a
// peer that stops sending is abandoned.
func StreamingClient() *http.Client {
	return &http.Client{Transport: sharedTransport}
}

// Get issues a GET for url and returns the response with its body wrapped in a
// stall watchdog.
//
// A stall greater than zero arms the watchdog: the response body must deliver
// at least one byte within every stall window, counted from the moment the
// headers arrive. When it does not, the request is canceled — which is what
// unblocks a read already parked in the transport — and the read fails with an
// error wrapping ErrStalled. A stall of zero or less installs no watchdog, for
// a caller whose client already carries a flat Timeout.
//
// The caller closes resp.Body exactly as it would for any response; that
// release also stops the watchdog and frees the request. There is no second
// cleanup function to forget.
func Get(ctx context.Context, client *http.Client, url string, stall time.Duration) (*http.Response, error) {
	ctx, cancel := context.WithCancel(ctx)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("build request for %s: %w", url, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = guard(resp.Body, cancel, stall)
	return resp, nil
}

// stallGuard wraps a response body and abandons the transfer when no bytes
// arrive for the budget.
//
// The interruption is a context cancel rather than a read deadline because a
// net/http response body exposes no deadline of its own: the only documented
// way to unpark a read that is already blocked in the transport is to cancel
// the request that produced it. Cancellation therefore does double duty here —
// it is both the alarm and the way the alarm is heard.
type stallGuard struct {
	body   io.ReadCloser
	cancel context.CancelFunc
	stall  time.Duration

	mu      sync.Mutex
	timer   *time.Timer
	stalled bool
	closed  bool
}

// guard wraps body, arming the watchdog only when a positive budget is given.
func guard(body io.ReadCloser, cancel context.CancelFunc, stall time.Duration) io.ReadCloser {
	g := &stallGuard{body: body, cancel: cancel, stall: stall}
	if stall > 0 {
		g.timer = time.AfterFunc(stall, g.trip)
	}
	return g
}

// Read forwards to the wrapped body and restarts the budget on any progress.
//
// A failure that follows a trip is reported as ErrStalled rather than as the
// cancellation it literally is. The underlying error would say "context
// canceled", which reads like the user pressed Ctrl-C.
func (g *stallGuard) Read(p []byte) (int, error) {
	n, err := g.body.Read(p)
	if n > 0 {
		g.kick()
	}
	if err != nil && g.tripped() {
		return n, fmt.Errorf("%w: no data received for %s", ErrStalled, g.stall)
	}
	return n, err
}

// Close releases the body, stops the watchdog and frees the request.
func (g *stallGuard) Close() error {
	g.mu.Lock()
	g.closed = true
	if g.timer != nil {
		g.timer.Stop()
	}
	g.mu.Unlock()

	err := g.body.Close()
	g.cancel()
	return err
}

// kick restarts the budget after progress. A guard that has already tripped is
// left tripped: the request is canceled by then, so pretending otherwise would
// only replace a clear diagnosis with a confusing one.
func (g *stallGuard) kick() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.timer != nil && !g.stalled && !g.closed {
		g.timer.Reset(g.stall)
	}
}

// trip records the stall and cancels the request. The cancel is issued outside
// the lock so it can never deadlock against a concurrent Close.
func (g *stallGuard) trip() {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return
	}
	g.stalled = true
	g.mu.Unlock()
	g.cancel()
}

// tripped reports whether the watchdog fired.
func (g *stallGuard) tripped() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stalled
}
