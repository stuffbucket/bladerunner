package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stuffbucket/bladerunner/internal/control"
	"github.com/stuffbucket/bladerunner/internal/instance"
)

// wedgedHolder is a stand-in for the failure `br stop --force` exists to
// recover from: a holder process that is alive, whose control socket is bound
// and accepting, and which never replies to anything.
//
// It is the state internal/instance already names ProcessOnly. control.Client's
// ping cannot tell it apart from a dead instance — the ping just times out — so
// a stop path that asks IsRunning() and nothing else reports "VM is not
// running" while the VM still holds its disk, its ports and its cartridge.
type wedgedHolder struct {
	stateDir   string
	socketPath string
	pid        int
}

// shortStateDir returns a state dir short enough to hold a unix socket path.
// macOS caps sun_path at 104 bytes and t.TempDir() alone is already close to it.
func shortStateDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "brstop")
	if err != nil {
		t.Fatalf("temp state dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// startWedgedHolder binds a control socket that accepts and never answers, and
// starts a real child process to stand in for the holder, recording its PID in
// the start lock exactly as a holder does.
func startWedgedHolder(t *testing.T) wedgedHolder {
	t.Helper()
	dir := shortStateDir(t)

	socketPath := control.SocketPath(dir)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("bind control socket: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		var held []net.Conn
		defer func() {
			for _, c := range held {
				_ = c.Close()
			}
		}()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Accepted, never read, never answered: the wedge.
			held = append(held, conn)
		}
	}()

	// A process that outlives the test and dies on SIGTERM, so the escalation
	// ladder has something real to signal.
	holder := exec.Command("/bin/sleep", "60")
	if err := holder.Start(); err != nil {
		t.Fatalf("start stand-in holder: %v", err)
	}
	// ONE waiter. os/exec panics on a second Wait, and the reap has to happen
	// off this goroutine so waitForProcessGone sees the process actually leave
	// the table rather than linger as a zombie that kill(pid, 0) still finds.
	reaped := make(chan struct{})
	go func() {
		_ = holder.Wait()
		close(reaped)
	}()
	t.Cleanup(func() {
		_ = holder.Process.Kill()
		<-reaped
	})

	pid := holder.Process.Pid
	if err := os.WriteFile(control.LockPath(dir), []byte(fmt.Sprintf("%d\n", pid)), 0o600); err != nil {
		t.Fatalf("write control lock: %v", err)
	}

	t.Setenv("BLADERUNNER_STATE_DIR", dir)
	return wedgedHolder{stateDir: dir, socketPath: socketPath, pid: pid}
}

// useStopFlags sets the `br stop` flags for one test and restores them after.
func useStopFlags(t *testing.T, force bool) {
	t.Helper()
	saved := stopFlags
	t.Cleanup(func() { stopFlags = saved })
	stopFlags.force = force
	stopFlags.timeout = 1
}

// TestRunStopForceTerminatesAWedgedHolder is the regression test for the bug
// that made --force unreachable in the one scenario it was written for.
//
// runStop gated everything on client.IsRunning(), which is a ping round trip
// rather than a liveness check, so a wedged holder failed the gate and runStop
// returned "VM is not running" — leaving forceTerminate, the whole
// SIGTERM/SIGKILL ladder, dead code. The user was then told the VM was not
// running by `br stop` and that "another bladerunner process holds this
// instance" by `br up`, with no CLI way out.
func TestRunStopForceTerminatesAWedgedHolder(t *testing.T) {
	h := startWedgedHolder(t)
	useStopFlags(t, true)

	if control.NewClient(h.stateDir).IsRunning() {
		t.Fatal("the stand-in holder answered a ping; it is not wedged")
	}
	if !instance.ProcessAlive(h.pid) {
		t.Fatalf("stand-in holder %d is not alive", h.pid)
	}

	if err := runStop(nil, nil); err != nil {
		t.Fatalf("runStop --force on a wedged holder = %v, want nil (it must reach forceTerminate)", err)
	}

	if instance.ProcessAlive(h.pid) {
		t.Errorf("holder process %d is still alive after 'br stop --force'", h.pid)
	}
	if _, err := os.Stat(h.socketPath); !os.IsNotExist(err) {
		t.Errorf("stale control socket %s was left behind", h.socketPath)
	}
}

// TestRunStopReportsAWedgedHolderWithoutForce holds the other half of the
// contract: without --force, stop must not claim the VM is not running. It has
// to name the state and the remedy, because the holder still owns everything it
// owned a moment ago.
func TestRunStopReportsAWedgedHolderWithoutForce(t *testing.T) {
	h := startWedgedHolder(t)
	useStopFlags(t, false)

	err := runStop(nil, nil)
	if err == nil {
		t.Fatal("runStop on a wedged holder = nil, want an error describing the wedge")
	}
	got := err.Error()
	if strings.Contains(got, "VM is not running") {
		t.Errorf("error = %q; the holder is alive, so this report is false", got)
	}
	for _, want := range []string{"--force", fmt.Sprint(h.pid)} {
		if !strings.Contains(got, want) {
			t.Errorf("error = %q, want it to contain %q", got, want)
		}
	}

	if !instance.ProcessAlive(h.pid) {
		t.Errorf("holder process %d was terminated without --force", h.pid)
	}
}

// TestRunStopReportsNotRunningForADeadHolder is the guard on the fix: a stale
// socket file left behind by a holder that died must still report "not
// running", and must not send a signal anywhere. A PID recorded next to a
// socket is only evidence while the process behind it exists.
func TestRunStopReportsNotRunningForADeadHolder(t *testing.T) {
	dir := shortStateDir(t)
	t.Setenv("BLADERUNNER_STATE_DIR", dir)
	useStopFlags(t, true)

	// A socket file with nothing bound to it, and a lock naming a PID that has
	// already exited.
	if err := os.WriteFile(control.SocketPath(dir), nil, 0o600); err != nil {
		t.Fatalf("write stale socket: %v", err)
	}
	dead := exec.Command("/bin/sh", "-c", "exit 0")
	if err := dead.Run(); err != nil {
		t.Fatalf("run short-lived process: %v", err)
	}
	if err := os.WriteFile(control.LockPath(dir), []byte(fmt.Sprintf("%d\n", dead.Process.Pid)), 0o600); err != nil {
		t.Fatalf("write control lock: %v", err)
	}

	err := runStop(nil, nil)
	if err == nil || !strings.Contains(err.Error(), "VM is not running") {
		t.Fatalf("runStop = %v, want an error saying the VM is not running", err)
	}
}

// TestHolderPIDPrefersTheStartLock holds that the PID used by the force path
// comes from a source that does not need the control socket to answer.
// readHostPID asks the control server, which is exactly what is not replying.
func TestHolderPIDPrefersTheStartLock(t *testing.T) {
	dir := shortStateDir(t)
	if err := os.WriteFile(control.LockPath(dir), []byte("4242\n"), 0o600); err != nil {
		t.Fatalf("write control lock: %v", err)
	}

	if got := holderPID(resolvedInstance{StateDir: dir, PID: 99}); got != 4242 {
		t.Errorf("holderPID = %d, want 4242 (the start lock next to the socket)", got)
	}

	// With no lock the registry record is the fallback.
	if err := os.Remove(control.LockPath(dir)); err != nil {
		t.Fatalf("remove control lock: %v", err)
	}
	if got := holderPID(resolvedInstance{StateDir: dir, PID: 99}); got != 99 {
		t.Errorf("holderPID = %d, want 99 (the registry record)", got)
	}
	if got := holderPID(resolvedInstance{StateDir: dir}); got != 0 {
		t.Errorf("holderPID = %d, want 0 when nothing records one", got)
	}
}

func TestDrainBudget(t *testing.T) {
	tests := []struct {
		name    string
		seconds int
		want    time.Duration
	}{
		{name: "explicit timeout is honored", seconds: 90, want: 90 * time.Second},
		{name: "zero falls back to the default", seconds: 0, want: control.DefaultEjectTimeoutSeconds * time.Second},
		{name: "negative falls back to the default", seconds: -5, want: control.DefaultEjectTimeoutSeconds * time.Second},
		{name: "oversized budget is capped", seconds: 3600, want: maxDrainRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := drainBudget(tt.seconds); got != tt.want {
				t.Errorf("drainBudget(%d) = %s, want %s", tt.seconds, got, tt.want)
			}
		})
	}
}

func TestWaitForStop(t *testing.T) {
	newSocket := func(t *testing.T) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "control.sock")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("create socket stand-in: %v", err)
		}
		return path
	}

	t.Run("reports stopped once the socket disappears", func(t *testing.T) {
		path := newSocket(t)
		reqErr := make(chan error, 1)
		go func() {
			time.Sleep(20 * time.Millisecond)
			_ = os.Remove(path)
			reqErr <- nil
		}()

		stopped, err := waitForStop(path, 2*time.Second, reqErr)
		if !stopped || err != nil {
			t.Fatalf("waitForStop = (%v, %v), want (true, nil)", stopped, err)
		}
	})

	t.Run("returns the request error instead of burning the budget", func(t *testing.T) {
		path := newSocket(t)
		reqErr := make(chan error, 1)
		want := errors.New("VM is not started yet")
		reqErr <- want

		start := time.Now()
		stopped, err := waitForStop(path, 10*time.Second, reqErr)
		if stopped {
			t.Fatal("waitForStop reported stopped while the socket still exists")
		}
		if !errors.Is(err, want) {
			t.Fatalf("err = %v, want %v", err, want)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("waited %s for a failed request; should return promptly", elapsed)
		}
	})

	t.Run("keeps waiting after a successful request", func(t *testing.T) {
		path := newSocket(t)
		reqErr := make(chan error, 1)
		reqErr <- nil

		stopped, err := waitForStop(path, 100*time.Millisecond, reqErr)
		if stopped {
			t.Fatal("waitForStop reported stopped while the socket still exists")
		}
		if err != nil {
			t.Fatalf("err = %v, want nil (plain timeout)", err)
		}
	})

	t.Run("succeeds when the socket is already gone", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "absent.sock")
		stopped, err := waitForStop(path, time.Second, make(chan error, 1))
		if !stopped || err != nil {
			t.Fatalf("waitForStop = (%v, %v), want (true, nil)", stopped, err)
		}
	})
}

// A stale socket plus a RECYCLED PID must not be mistaken for a wedged holder.
//
// A holder killed with SIGKILL leaves the socket inode and the lock file behind.
// Once the OS reuses its PID, "socket file present and PID alive" becomes true
// of an innocent process -- so a guard built on os.Stat hands --force somebody
// else's process to terminate. Only a successful connect distinguishes a live
// wedged holder from a dead one's litter.
//
// TestRunStopReportsNotRunningForADeadHolder covers the easier half, where the
// recorded PID has exited. This is the half where it has not.
func TestRunStopDoesNotSignalARecycledPID(t *testing.T) {
	dir := shortStateDir(t)
	t.Setenv("BLADERUNNER_STATE_DIR", dir)
	useStopFlags(t, true)

	// A crashed holder's leavings: a socket FILE with nothing listening.
	if err := os.WriteFile(control.SocketPath(dir), nil, 0o600); err != nil {
		t.Fatalf("write stale socket: %v", err)
	}

	// An unrelated live process standing where the holder's PID used to be.
	innocent := exec.Command("/bin/sleep", "60")
	if err := innocent.Start(); err != nil {
		t.Fatalf("start innocent process: %v", err)
	}
	reaped := make(chan struct{})
	go func() { _ = innocent.Wait(); close(reaped) }()
	t.Cleanup(func() {
		_ = innocent.Process.Kill()
		<-reaped
	})
	pid := innocent.Process.Pid

	if err := os.WriteFile(control.LockPath(dir), fmt.Appendf(nil, "%d\n", pid), 0o600); err != nil {
		t.Fatalf("write control lock: %v", err)
	}

	err := runStop(nil, nil)
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("runStop --force = %v, want it to report the instance is not running", err)
	}
	if !instance.ProcessAlive(pid) {
		t.Errorf("br stop --force terminated pid %d, which is not a bladerunner holder", pid)
	}
}
