//go:build darwin

package main

import (
	"os"
	"testing"
	"time"
)

// isolatedStateDir points DefaultStateDir at a SHORT temp dir for the test via
// the BLADERUNNER_STATE_DIR env var, so the menubar socket never collides with
// a real install or another test. We avoid t.TempDir(): on macOS it lives under
// $TMPDIR (/var/folders/...) and, with the long test name appended, the
// resulting socket path can exceed the 104-byte sun_path limit (production's
// ~/.local/state/bladerunner is well under). A short /tmp base keeps it valid.
func isolatedStateDir(t *testing.T) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "br-mb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("BLADERUNNER_STATE_DIR", dir)
}

func TestAcquireMenubarLockFirstWins(t *testing.T) {
	isolatedStateDir(t)

	release, already := acquireMenubarLock(func() {})
	if already {
		t.Fatal("first acquire reported already-running")
	}
	if release == nil {
		t.Fatal("first acquire returned nil release")
	}
	defer release()

	if _, err := os.Stat(menubarSocketPath()); err != nil {
		t.Errorf("socket not created: %v", err)
	}
}

func TestAcquireMenubarLockSecondHandsOff(t *testing.T) {
	isolatedStateDir(t)

	presented := make(chan struct{}, 1)
	release, already := acquireMenubarLock(func() { presented <- struct{}{} })
	if already {
		t.Fatal("first acquire reported already-running")
	}
	defer release()

	// A second acquire must detect the first and hand off (already=true).
	release2, already2 := acquireMenubarLock(func() { t.Error("second instance should not serve") })
	if !already2 {
		t.Fatal("second acquire did not detect the running instance")
	}
	if release2 != nil {
		t.Error("second acquire should return a nil release (it owns nothing)")
	}

	select {
	case <-presented:
		// good: the running instance was asked to surface.
	case <-time.After(2 * time.Second):
		t.Fatal("running instance never received the present handoff")
	}
}

func TestAcquireMenubarLockReleaseAllowsReacquire(t *testing.T) {
	isolatedStateDir(t)

	release, already := acquireMenubarLock(func() {})
	if already {
		t.Fatal("first acquire reported already-running")
	}
	release()

	// The socket should be gone after release, so a fresh acquire wins again.
	if _, err := os.Stat(menubarSocketPath()); !os.IsNotExist(err) {
		t.Errorf("socket not removed on release: stat err = %v", err)
	}
	release2, already2 := acquireMenubarLock(func() {})
	if already2 {
		t.Fatal("re-acquire after release reported already-running")
	}
	release2()
}

func TestAcquireMenubarLockStaleSocket(t *testing.T) {
	isolatedStateDir(t)

	// Simulate a crashed instance: a socket file with no listener behind it.
	path := menubarSocketPath()
	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	release, already := acquireMenubarLock(func() {})
	if already {
		t.Fatal("a stale socket file should not look like a running instance")
	}
	t.Cleanup(release)
}
