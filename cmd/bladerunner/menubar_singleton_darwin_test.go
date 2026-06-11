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

func noop() {}

func TestShouldStepDown(t *testing.T) {
	tests := []struct {
		mine, theirs string
		want         bool
	}{
		{"0.5.0", "0.4.7", true},   // newer -> take over
		{"0.4.7", "0.5.0", false},  // older -> defer
		{"0.4.7", "0.4.7", false},  // equal -> defer
		{"v0.5.0", "v0.4.0", true}, // v-prefix tolerated
		{"1.0.0", "0.9.9", true},
		{"dev", "0.4.7", false}, // unknown launcher -> defer
		{"0.5.0", "dev", false}, // unknown running -> defer
		{"", "0.4.7", false},    // empty -> defer
		{"0.4.7", "garbage", false},
	}
	for _, tt := range tests {
		if got := shouldStepDown(tt.mine, tt.theirs); got != tt.want {
			t.Errorf("shouldStepDown(%q,%q)=%v, want %v", tt.mine, tt.theirs, got, tt.want)
		}
	}
}

func TestAcquireMenubarLockFirstWins(t *testing.T) {
	isolatedStateDir(t)

	release, already := acquireMenubarLock("1.0.0", noop, noop)
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

// A same-version second launch defers and asks the running instance to surface.
func TestAcquireMenubarLockSameVersionDefers(t *testing.T) {
	isolatedStateDir(t)

	presented := make(chan struct{}, 1)
	release, already := acquireMenubarLock("1.0.0", func() { presented <- struct{}{} }, noop)
	if already {
		t.Fatal("first acquire reported already-running")
	}
	defer release()

	release2, already2 := acquireMenubarLock("1.0.0", func() { t.Error("second should not serve") }, noop)
	if !already2 {
		t.Fatal("same-version second acquire did not defer")
	}
	if release2 != nil {
		t.Error("deferring second acquire should return a nil release")
	}
	select {
	case <-presented:
	case <-time.After(2 * time.Second):
		t.Fatal("running instance never received the present handoff")
	}
}

// An OLDER second launch defers to the newer running instance.
func TestAcquireMenubarLockOlderDefers(t *testing.T) {
	isolatedStateDir(t)

	release, already := acquireMenubarLock("1.1.0", noop, func() { t.Error("newer instance must not step down for an older one") })
	if already {
		t.Fatal("first acquire reported already-running")
	}
	defer release()

	release2, already2 := acquireMenubarLock("1.0.0", noop, noop)
	if !already2 {
		t.Fatal("older second acquire should defer")
	}
	if release2 != nil {
		t.Error("deferring acquire should return nil release")
	}
	// Give any (erroneous) stepdown a moment to fire.
	time.Sleep(150 * time.Millisecond)
}

// A NEWER second launch (an upgrade) makes the running instance step down and
// takes over the socket.
func TestAcquireMenubarLockNewerStepsDown(t *testing.T) {
	isolatedStateDir(t)

	var release1 func()
	steppedDown := make(chan struct{}, 1)
	r1, already := acquireMenubarLock("1.0.0", noop, func() {
		release1() // a real instance would systray.Quit -> release on exit
		steppedDown <- struct{}{}
	})
	if already {
		t.Fatal("first acquire reported already-running")
	}
	release1 = r1

	// Newer launch should take over (already=false) and own the socket.
	release2, already2 := acquireMenubarLock("1.1.0", noop, noop)
	if already2 {
		t.Fatal("newer launch should take over, not defer")
	}
	if release2 == nil {
		t.Fatal("newer launch should own the socket (non-nil release)")
	}
	defer release2()

	select {
	case <-steppedDown:
	case <-time.After(3 * time.Second):
		t.Fatal("running instance was never asked to step down")
	}
	if _, err := os.Stat(menubarSocketPath()); err != nil {
		t.Errorf("new instance does not hold the socket: %v", err)
	}
}

func TestAcquireMenubarLockReleaseAllowsReacquire(t *testing.T) {
	isolatedStateDir(t)

	release, already := acquireMenubarLock("1.0.0", noop, noop)
	if already {
		t.Fatal("first acquire reported already-running")
	}
	release()

	if _, err := os.Stat(menubarSocketPath()); !os.IsNotExist(err) {
		t.Errorf("socket not removed on release: stat err = %v", err)
	}
	release2, already2 := acquireMenubarLock("1.0.0", noop, noop)
	if already2 {
		t.Fatal("re-acquire after release reported already-running")
	}
	release2()
}

func TestAcquireMenubarLockStaleSocket(t *testing.T) {
	isolatedStateDir(t)

	// Simulate a crashed instance: a socket file with no listener behind it.
	if err := os.WriteFile(menubarSocketPath(), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	release, already := acquireMenubarLock("1.0.0", noop, noop)
	if already {
		t.Fatal("a stale socket file should not look like a running instance")
	}
	t.Cleanup(release)
}
