package webproxy

import (
	"bufio"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"
)

// testIdleTimeout is the idle bound this test substitutes so the reaping is
// observable in a test rather than in two minutes.
const testIdleTimeout = 200 * time.Millisecond

// idleReadDeadline is how long the test waits for the server to close an idle
// connection. Generously longer than testIdleTimeout so a loaded machine does
// not turn a pass into a flake.
const idleReadDeadline = 10 * time.Second

// TestReapsIdleKeepAliveConnections holds #285.
//
// The first half is the regression guard: IdleTimeout must be set, and
// ReadTimeout/WriteTimeout must not be. Go falls back to ReadTimeout for the
// idle bound, so a server with neither never reaps an idle keep-alive
// connection — every browser tab that ever opened the Incus UI kept a
// descriptor and a goroutine on the holder. WriteTimeout is the one that must
// stay absent: it is a whole-response deadline and this proxy carries the Incus
// event stream and the console WebSockets, which a deadline would cut off.
//
// The second half proves the bound actually closes an idle connection rather
// than merely being present in a struct, with the timeout shortened so the test
// does not have to wait out the real one.
func TestReapsIdleKeepAliveConnections(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	dir := t.TempDir()
	p, err := New(Options{
		ListenAddr:   "127.0.0.1:0",
		UpstreamAddr: u.Host,
		CertPath:     filepath.Join(dir, "proxy.crt"),
		KeyPath:      filepath.Join(dir, "proxy.key"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if p.srv.IdleTimeout <= 0 {
		t.Error("IdleTimeout is unset, so idle keep-alive connections are never reaped " +
			"(Go falls back to ReadTimeout, which is unset too)")
	}
	if p.srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout is %v; it must stay unset, or the Incus event stream and "+
			"console WebSockets are cut off at the deadline", p.srv.WriteTimeout)
	}
	if p.srv.ReadTimeout != 0 {
		t.Errorf("ReadTimeout is %v; it is a whole-request deadline and would cut off "+
			"streamed request bodies. ReadHeaderTimeout is the Slowloris bound", p.srv.ReadTimeout)
	}

	// Shorten the bound so its effect is observable now. Set before Start, so
	// the server never serves with the production value — and only when it was
	// set at all, so a server missing the bound is served as-is and the read
	// below sees the connection held open rather than reaped.
	if p.srv.IdleTimeout > 0 {
		p.srv.IdleTimeout = testIdleTimeout
	}

	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	conn, err := tls.Dial("tcp", p.listenAddr(), &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // test client to the self-signed proxy
	if err != nil {
		t.Fatalf("dial the proxy: %v", err)
	}
	defer conn.Close()

	req, err := http.NewRequest(http.MethodGet, "https://"+p.listenAddr()+"/", http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if err := req.Write(conn); err != nil {
		t.Fatalf("write request: %v", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("drain response: %v", err)
	}
	_ = resp.Body.Close()
	if resp.Close {
		t.Fatal("the proxy did not keep the connection alive, so there is no idle connection to reap")
	}

	// The connection is now idle. The server owes us a close.
	if err := conn.SetReadDeadline(time.Now().Add(idleReadDeadline)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, err := br.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("idle connection read returned %v, want io.EOF from the server closing it", err)
	}
}
