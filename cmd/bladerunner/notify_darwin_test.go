//go:build darwin

package main

import (
	"testing"
	"time"
)

// fakeNotifier records the banners emitted so tests can assert the exact
// sequence of transitions that produced a notification.
type fakeNotifier struct{ bodies []string }

func (f *fakeNotifier) notify(_, body string) { f.bodies = append(f.bodies, body) }

// fakeSplash records Show/Hide calls.
type fakeSplash struct {
	shows int
	hides int
}

func (s *fakeSplash) Show() { s.shows++ }
func (s *fakeSplash) Hide() { s.hides++ }

// harness builds a vmNotifier with fakes and a controllable clock.
type harness struct {
	m  *vmNotifier
	n  *fakeNotifier
	sp *fakeSplash
	t0 time.Time
}

func newHarness() *harness {
	n := &fakeNotifier{}
	sp := &fakeSplash{}
	// A fixed, non-zero base time; Date.now() isn't available but explicit
	// construction from a Unix epoch is.
	t0 := time.Unix(1_700_000_000, 0)
	return &harness{m: newVMNotifier(n, sp), n: n, sp: sp, t0: t0}
}

// at returns t0 + the given offset.
func (h *harness) at(d time.Duration) time.Time { return h.t0.Add(d) }

func TestNotifySeedNoBanner(t *testing.T) {
	h := newHarness()
	// First reading seeds state; nothing should fire even if it's healthy.
	h.m.observe(vmHealthy, h.at(0))
	if len(h.n.bodies) != 0 {
		t.Errorf("seed emitted banners: %v", h.n.bodies)
	}
	if h.sp.hides == 0 {
		t.Error("seeding on an already-healthy VM should hide any stale splash")
	}
}

func TestNotifyReadyOnStartedToHealthy(t *testing.T) {
	h := newHarness()
	h.m.observe(vmStopped, h.at(0)) // seed: stopped
	h.m.onStart(h.at(1 * time.Second))
	// Boot passes through transient unknown/wedged (suppressed), then healthy.
	h.m.observe(vmUnknown, h.at(4*time.Second))
	h.m.observe(vmWedged, h.at(7*time.Second)) // within suppress window -> ignored
	h.m.observe(vmHealthy, h.at(20*time.Second))

	if got := h.n.bodies; len(got) != 1 || got[0] != bodyReady {
		t.Fatalf("bodies = %v, want one 'ready'", got)
	}
	if h.sp.shows != 1 {
		t.Errorf("splash shows = %d, want 1 (onStart)", h.sp.shows)
	}
	if h.sp.hides == 0 {
		t.Error("splash should hide on the healthy edge")
	}
}

func TestNotifyWedgedDebounced(t *testing.T) {
	h := newHarness()
	h.m.observe(vmHealthy, h.at(0)) // seed healthy
	// A single wedged reading must NOT notify (needs debounceReads=2).
	h.m.observe(vmWedged, h.at(40*time.Second))
	if len(h.n.bodies) != 0 {
		t.Fatalf("single wedged read notified: %v", h.n.bodies)
	}
	// Second consecutive wedged commits the transition.
	h.m.observe(vmWedged, h.at(43*time.Second))
	if got := h.n.bodies; len(got) != 1 || got[0] != bodyUnresponsive {
		t.Fatalf("bodies = %v, want one 'unresponsive'", got)
	}
	// Staying wedged must not re-notify (one per episode).
	h.m.observe(vmWedged, h.at(60*time.Second))
	h.m.observe(vmWedged, h.at(70*time.Second))
	if len(h.n.bodies) != 1 {
		t.Errorf("re-notified while staying wedged: %v", h.n.bodies)
	}
}

func TestNotifyUnknownHolds(t *testing.T) {
	h := newHarness()
	h.m.observe(vmHealthy, h.at(0)) // seed healthy
	// Repeated unknowns must never notify and must not become the committed
	// state, so a later healthy reading is a no-op (not a stopped->healthy edge).
	for i := 1; i <= 5; i++ {
		h.m.observe(vmUnknown, h.at(time.Duration(i)*time.Second))
	}
	h.m.observe(vmHealthy, h.at(30*time.Second))
	if len(h.n.bodies) != 0 {
		t.Errorf("unknown/healthy churn emitted banners: %v", h.n.bodies)
	}
}

func TestNotifyStoppedFromHealthy(t *testing.T) {
	h := newHarness()
	h.m.observe(vmHealthy, h.at(0))
	h.m.observe(vmStopped, h.at(30*time.Second))
	if got := h.n.bodies; len(got) != 1 || got[0] != bodyStopped {
		t.Fatalf("bodies = %v, want one 'stopped'", got)
	}
}

func TestNotifyRecoveredFromWedged(t *testing.T) {
	h := newHarness()
	h.m.observe(vmHealthy, h.at(0))
	h.m.observe(vmWedged, h.at(40*time.Second))
	h.m.observe(vmWedged, h.at(43*time.Second)) // commit wedged -> "unresponsive"
	h.m.observe(vmHealthy, h.at(80*time.Second))
	got := h.n.bodies
	if len(got) != 2 || got[1] != bodyRecovered {
		t.Fatalf("bodies = %v, want [..., 'recovered']", got)
	}
}

func TestNotifyRateLimit(t *testing.T) {
	h := newHarness()
	h.m.minInterval = 10 * time.Second
	h.m.observe(vmStopped, h.at(0)) // seed
	h.m.onStart(h.at(1 * time.Second))
	h.m.observe(vmHealthy, h.at(40*time.Second)) // ready (t=40s)
	// A stop 5s later is within the 10s rate-limit window -> suppressed banner,
	// but the state still commits.
	h.m.observe(vmStopped, h.at(45*time.Second))
	if got := h.n.bodies; len(got) != 1 || got[0] != bodyReady {
		t.Fatalf("bodies = %v, want only 'ready' (stop rate-limited)", got)
	}
	// A later transition outside the window notifies again, proving the state
	// committed (stopped) so this is stopped->healthy.
	h.m.observe(vmHealthy, h.at(60*time.Second))
	if got := h.n.bodies; len(got) != 2 || got[1] != bodyReady {
		t.Fatalf("bodies = %v, want second 'ready'", got)
	}
}

func TestNotifyWakeBanner(t *testing.T) {
	h := newHarness()
	h.m.onWake(h.at(0))
	if got := h.n.bodies; len(got) != 1 || got[0] != bodyReconnecting {
		t.Fatalf("bodies = %v, want one 'reconnecting'", got)
	}
	// Within the rate-limit window a second wake is suppressed.
	h.m.onWake(h.at(2 * time.Second))
	if len(h.n.bodies) != 1 {
		t.Errorf("wake banner not rate-limited: %v", h.n.bodies)
	}
}

func TestNotifySuppressWedgedExpires(t *testing.T) {
	h := newHarness()
	h.m.observe(vmStopped, h.at(0))
	h.m.onStart(h.at(0))
	// Two wedged reads INSIDE the suppress window: ignored.
	h.m.observe(vmWedged, h.at(5*time.Second))
	h.m.observe(vmWedged, h.at(8*time.Second))
	if len(h.n.bodies) != 0 {
		t.Fatalf("wedged within suppress window notified: %v", h.n.bodies)
	}
	// After the window, two wedged reads commit. prev state is still stopped
	// (wedged never committed during suppression), and stopped->wedged is not a
	// notify-worthy transition, so still nothing — but it must not panic and the
	// machine stays consistent.
	h.m.observe(vmWedged, h.at(40*time.Second))
	h.m.observe(vmWedged, h.at(43*time.Second))
	if len(h.n.bodies) != 0 {
		t.Errorf("unexpected banner after suppression: %v", h.n.bodies)
	}
}

func TestIsAppBundlePath(t *testing.T) {
	tests := []struct {
		exe  string
		want bool
	}{
		{"/Users/x/Applications/Bladerunner.app/Contents/MacOS/Bladerunner", true},
		{"/opt/homebrew/bin/br", false},
		{"/usr/local/bin/br", false},
		{"/tmp/build/br", false},
		{"/Applications/Other.app/Contents/MacOS/Other", true},
		{"", false},
	}
	for _, tt := range tests {
		if got := isAppBundlePath(tt.exe); got != tt.want {
			t.Errorf("isAppBundlePath(%q) = %v, want %v", tt.exe, got, tt.want)
		}
	}
}

// defaultNotifier/defaultSplash must return non-nil controllers. Outside a .app
// bundle (as in tests) defaultNotifier is the no-op, so driving the machine
// through a transition must not panic.
func TestDefaultsAreNoops(t *testing.T) {
	if defaultNotifier() == nil || defaultSplash() == nil {
		t.Fatal("defaultNotifier/defaultSplash must return non-nil controllers")
	}
	m := newVMNotifier(defaultNotifier(), defaultSplash())
	now := time.Unix(1_700_000_000, 0)
	m.onStart(now)
	m.observe(vmStopped, now)
	m.observe(vmHealthy, now.Add(time.Second))
	m.onWake(now.Add(2 * time.Second)) // must not panic
}
