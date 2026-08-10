package control_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/control"
	"github.com/stuffbucket/bladerunner/internal/instance"
)

// TestLockOwnerPID holds the contract that the holder of an instance can be
// named without the control socket answering. That is the whole point of the
// function: `br stop --force` needs a PID for a holder that is alive and wedged,
// and asking the wedged server for its own PID returns nothing.
func TestLockOwnerPID(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(control.LockPath(dir), []byte("4242\n"), 0o600); err != nil {
		t.Fatalf("write control lock: %v", err)
	}
	pid, err := control.LockOwnerPID(dir)
	if err != nil {
		t.Fatalf("LockOwnerPID: %v", err)
	}
	if pid != 4242 {
		t.Errorf("LockOwnerPID = %d, want 4242", pid)
	}

	// No lock file: a directory that never held one, or a holder that exited.
	if _, err := control.LockOwnerPID(filepath.Join(dir, "absent")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("LockOwnerPID on a missing lock = %v, want a wrapped fs.ErrNotExist", err)
	}

	// A record that is not a process number is not a holder.
	if err := os.WriteFile(control.LockPath(dir), []byte("not-a-pid\n"), 0o600); err != nil {
		t.Fatalf("write control lock: %v", err)
	}
	if pid, err := control.LockOwnerPID(dir); err == nil {
		t.Errorf("LockOwnerPID on a corrupt lock = (%d, nil), want an error", pid)
	}
}

// TestLockOwnerPIDNamesTheProcessThatBoundTheSocket holds the claim the doc
// comment makes about a different component: the lock a real listener takes
// records the PID of the process serving that socket, and it is gone once the
// listener closes.
func TestLockOwnerPIDNamesTheProcessThatBoundTheSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "brlock")
	if err != nil {
		t.Fatalf("temp state dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	l, err := control.NewListener(dir, nil)
	if err != nil {
		t.Fatalf("NewListener: %v", err)
	}

	pid, err := control.LockOwnerPID(dir)
	if err != nil {
		t.Fatalf("LockOwnerPID while serving: %v", err)
	}
	if pid != os.Getpid() {
		t.Errorf("LockOwnerPID = %d, want this process %d", pid, os.Getpid())
	}

	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := control.LockOwnerPID(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("LockOwnerPID after Close = %v, want a wrapped fs.ErrNotExist", err)
	}
}

// TestSocketPathAgreesWithTheRegistry holds the "keep the two in sync" comment
// on instance.controlSocketName.
//
// internal/instance cannot import this package — this package imports it — so
// it hand-copies the control socket's file name. A comment asking a future
// editor to keep two constants in sync is exactly the class of claim that goes
// wrong in silence (AGENTS.md section 5, point 7): if the two ever drift, the
// registry's liveness probe dials a path no listener ever binds and every
// instance reads as Dead. This test is the only thing that would notice.
func TestSocketPathAgreesWithTheRegistry(t *testing.T) {
	dir, err := os.MkdirTemp("", "brsock")
	if err != nil {
		t.Fatalf("temp state dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	if got, want := instance.ControlSocketPath(dir), control.SocketPath(dir); got != want {
		t.Fatalf("instance.ControlSocketPath = %q, control.SocketPath = %q; the hand-copied socket name has drifted", got, want)
	}

	// Not merely equal strings: the path a real listener binds has to be the one
	// the registry's probe dials, so a live instance reads as serving.
	l, err := control.NewListener(dir, nil)
	if err != nil {
		t.Fatalf("NewListener: %v", err)
	}
	defer func() { _ = l.Close() }()

	if !instance.DefaultProbe(instance.ControlSocketPath(dir)) {
		t.Error("instance.DefaultProbe found no listener at the path control.NewListener bound")
	}
}
