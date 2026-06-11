//go:build darwin

package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/blang/semver/v4"
	"github.com/stuffbucket/bladerunner/internal/config"
)

// The menubar runs as a singleton per host. This guard is SEPARATE from the VM
// runner's control socket (internal/control's <stateDir>/control.sock): a
// running VM with no menubar, and a menubar with no VM, are both valid states,
// so conflating the two would break both. We mirror control/listener.go's
// dial-first-else-bind idiom on our own socket.
//
// The handshake is version-aware. A second launch first probes the running
// instance for its version, then either:
//   - defers (launch version <= running): sends "present" so the running
//     instance re-surfaces, and exits 0; or
//   - takes over (launch version > running, an upgrade): sends "stepdown" so the
//     older instance quits, then binds and becomes the live menubar. Only the
//     UI process is swapped — the VM runs in a detached `br start` and is
//     untouched, so containers keep running.
//
// Each connection carries exactly one line: "hello <ver>" (reply: our version),
// "present", or "stepdown". Non-semver/dev versions never trigger a stepdown
// (compared conservatively), so only a clearly-newer build takes over.
const (
	menubarSocketName  = "menubar.sock"
	menubarDialTimeout = 200 * time.Millisecond
	menubarHelloVerb   = "hello"
	menubarPresentVerb = "present"
	menubarStepDown    = "stepdown"
	// stepdownWait bounds how long a taking-over instance waits for the older one
	// to release the socket before falling back to binding anyway / failing open.
	stepdownWait   = 5 * time.Second
	stepdownPoll   = 60 * time.Millisecond
	socketLineWait = time.Second
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

// shouldStepDown reports whether a launching build of version mine should take
// over a running instance of version theirs — true only when both parse as
// semver and mine is strictly newer. Dev/unknown versions (and downgrades or
// equal) defer, so an upgrade is the only thing that ever displaces a running
// menubar.
func shouldStepDown(mine, theirs string) bool {
	mv, err1 := semver.ParseTolerant(mine)
	tv, err2 := semver.ParseTolerant(theirs)
	if err1 != nil || err2 != nil {
		return false
	}
	return mv.GT(tv)
}

// acquireMenubarLock enforces the version-aware single-instance rule. onPresent
// re-surfaces this instance on a deferred handoff; onStepdown quits it when a
// newer instance takes over. Returns already=true when the caller should exit
// (it deferred to a running instance), or a release func when it owns the lock.
//
// It deliberately FAILS OPEN: if it can't bind (permissions oddity, a lost
// race), it returns already=false with a no-op release so the menubar still
// launches — a running menubar without the lock beats no menubar at all.
func acquireMenubarLock(myVersion string, onPresent, onStepdown func()) (release func(), already bool) {
	path := menubarSocketPath()
	noop := func() {}

	if theirVer, alive := probeMenubar(path, myVersion); alive {
		if !shouldStepDown(myVersion, theirVer) {
			// Same or older than the running instance: re-surface it and exit.
			sendMenubarVerb(path, menubarPresentVerb)
			return nil, true
		}
		// We're newer (an upgrade): ask the running instance to quit, then take
		// over its socket. The VM is detached, so this swaps only the UI.
		sendMenubarVerb(path, menubarStepDown)
		waitForMenubarRelease(path)
	}

	if rel, ok := bindMenubar(path, myVersion, onPresent, onStepdown); ok {
		return rel, false
	}

	// Lost a bind race. If someone now holds it and isn't older, defer; else run
	// without the lock rather than refuse to launch.
	if theirVer, alive := probeMenubar(path, myVersion); alive && !shouldStepDown(myVersion, theirVer) {
		sendMenubarVerb(path, menubarPresentVerb)
		return nil, true
	}
	return noop, false
}

// probeMenubar sends "hello <myVersion>" and returns the running instance's
// version, or alive=false if nothing answered.
func probeMenubar(path, myVersion string) (version string, alive bool) {
	conn, ok := dialMenubar(path)
	if !ok {
		return "", false
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(menubarDialTimeout))
	if _, err := fmt.Fprintf(conn, "%s %s\n", menubarHelloVerb, myVersion); err != nil {
		return "", true // alive but couldn't read its version; treat as present-only
	}
	line, _ := bufio.NewReader(conn).ReadString('\n')
	return strings.TrimSpace(line), true
}

// sendMenubarVerb sends a one-shot verb ("present"/"stepdown") to the running
// instance. Best-effort.
func sendMenubarVerb(path, verb string) {
	conn, ok := dialMenubar(path)
	if !ok {
		return
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetWriteDeadline(time.Now().Add(menubarDialTimeout))
	_, _ = conn.Write([]byte(verb + "\n"))
}

func dialMenubar(path string) (net.Conn, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), menubarDialTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, false
	}
	return conn, true
}

// waitForMenubarRelease polls until nothing is listening on the socket (the
// stepped-down instance has quit), up to stepdownWait. Returns false on timeout.
func waitForMenubarRelease(path string) bool {
	deadline := time.Now().Add(stepdownWait)
	for time.Now().Before(deadline) {
		if conn, ok := dialMenubar(path); ok {
			_ = conn.Close()
			time.Sleep(stepdownPoll)
			continue
		}
		return true
	}
	return false
}

// bindMenubar binds the socket and starts serving the handshake.
func bindMenubar(path, myVersion string, onPresent, onStepdown func()) (release func(), ok bool) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, false
	}
	_ = os.Remove(path) // clear any socket the released instance left behind
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "unix", path)
	if err != nil {
		return nil, false
	}
	_ = os.Chmod(path, 0o600)
	go serveMenubar(ln, myVersion, onPresent, onStepdown)
	return func() {
		_ = ln.Close()
		_ = os.Remove(path)
	}, true
}

// serveMenubar handles handshake connections until the listener is closed.
func serveMenubar(ln net.Listener, myVersion string, onPresent, onStepdown func()) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed on quit
		}
		go handleMenubarConn(conn, myVersion, onPresent, onStepdown)
	}
}

func handleMenubarConn(conn net.Conn, myVersion string, onPresent, onStepdown func()) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(socketLineWait))
	line, _ := bufio.NewReader(conn).ReadString('\n')
	line = strings.TrimSpace(line)

	switch {
	case strings.HasPrefix(line, menubarHelloVerb+" "), line == menubarHelloVerb:
		_, _ = fmt.Fprintf(conn, "%s\n", myVersion)
	case line == menubarPresentVerb:
		if onPresent != nil {
			onPresent()
		}
	case line == menubarStepDown:
		if onStepdown != nil {
			onStepdown()
		}
	}
}
