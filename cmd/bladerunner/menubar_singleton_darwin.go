//go:build darwin

package main

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/stuffbucket/bladerunner/internal/config"
)

// The menubar runs as a singleton per host. This guard is SEPARATE from the VM
// runner's control socket (internal/control's <stateDir>/control.sock): a
// running VM with no menubar, and a menubar with no VM, are both valid states,
// so conflating the two would break both. We mirror control/listener.go's
// dial-first-else-bind idiom on our own socket, and use it not just as a mutex
// but as a wakeup channel: a second launch sends "present" so the running
// instance can re-surface its splash.
const (
	menubarSocketName  = "menubar.sock"
	menubarDialTimeout = 200 * time.Millisecond
	menubarPresentVerb = "present"
)

// menubarPresent is the handler the running instance installs (via
// setMenubarPresentHandler in onMenubarReady) to re-surface itself when a second
// launch hands off. It is nil until the menubar is ready, so an early handoff is
// a safe no-op.
var (
	menubarPresentMu sync.Mutex
	menubarPresentFn func()
)

// setMenubarPresentHandler installs the function run when a second instance asks
// this one to surface (re-show the splash). Called once from onMenubarReady.
func setMenubarPresentHandler(fn func()) {
	menubarPresentMu.Lock()
	menubarPresentFn = fn
	menubarPresentMu.Unlock()
}

func firePresent() {
	menubarPresentMu.Lock()
	fn := menubarPresentFn
	menubarPresentMu.Unlock()
	if fn != nil {
		fn()
	}
}

func menubarSocketPath() string {
	return filepath.Join(config.DefaultStateDir(), menubarSocketName)
}

// acquireMenubarLock enforces the single-instance rule. It dials the menubar
// socket first: if a live instance answers, this is a second launch — it sends
// "present" (so the running instance re-surfaces) and returns already=true so
// the caller exits 0 quietly. Otherwise it binds the socket, serves "present"
// requests by invoking onPresent, and returns a release func to close it on
// quit.
//
// It deliberately FAILS OPEN: if the state dir or socket can't be created (a
// permissions oddity, a lost bind race), it returns already=false with a no-op
// release so the menubar still launches — a missing guard is better than a
// menubar that refuses to start.
func acquireMenubarLock(onPresent func()) (release func(), already bool) {
	path := menubarSocketPath()
	noop := func() {}

	// Dial-first: is an instance already listening?
	if dialPresent(path) {
		return nil, true
	}

	// No listener answered. Clear any stale socket file, then bind.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return noop, false // fail open
	}
	_ = os.Remove(path)
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "unix", path)
	if err != nil {
		// Likely lost a race to another launching instance. Try one more dial:
		// if it's now listening, defer to it; otherwise fail open.
		if dialPresent(path) {
			return nil, true
		}
		return noop, false
	}
	_ = os.Chmod(path, 0o600)

	go serveMenubarPresent(ln, onPresent)
	return func() {
		_ = ln.Close()
		_ = os.Remove(path)
	}, false
}

// dialPresent connects to a (possibly live) menubar socket and, on success,
// sends the present verb. Returns true iff a live instance answered.
func dialPresent(path string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), menubarDialTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetWriteDeadline(time.Now().Add(menubarDialTimeout))
	_, _ = conn.Write([]byte(menubarPresentVerb + "\n"))
	return true
}

// serveMenubarPresent accepts connections on the menubar socket and fires
// onPresent for each valid "present" message, until the listener is closed.
func serveMenubarPresent(ln net.Listener, onPresent func()) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed on quit
		}
		go handlePresentConn(conn, onPresent)
	}
}

func handlePresentConn(conn net.Conn, onPresent func()) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	line, _ := bufio.NewReader(conn).ReadString('\n')
	if strings.TrimSpace(line) == menubarPresentVerb && onPresent != nil {
		onPresent()
	}
}
