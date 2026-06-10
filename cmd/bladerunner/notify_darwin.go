//go:build darwin

package main

import (
	"sync"
	"time"
)

// notifyTitle is the title shown on every banner. The body carries the detail.
const notifyTitle = "Bladerunner"

// Banner bodies, as constants so the machine and its tests reference one source.
const (
	bodyReady        = "Your VM is ready."
	bodyRecovered    = "Your VM recovered and is responding again."
	bodyUnresponsive = "Your VM is unresponsive — try Restart."
	bodyStopped      = "Your VM stopped."
	bodyReconnecting = "Reconnecting after sleep…"
)

// Tuning for the transition state machine, sized against the 3s health poll.
const (
	// notifyDebounceReads is how many consecutive "wedged" readings must be
	// seen before we believe the guest is genuinely unresponsive (vs. a single
	// slow probe). 2 reads ≈ 6s.
	notifyDebounceReads = 2
	// notifySuppressAfterStart silences wedged/unknown notifications for a
	// window after a Start, so a slowly-booting guest doesn't toast a false
	// "unresponsive". A cold first boot can download/convert the image, but the
	// "ready" edge (stopped->healthy) still fires whenever it lands.
	notifySuppressAfterStart = 30 * time.Second
	// notifyMinInterval rate-limits banners so a flapping guest can't spam.
	notifyMinInterval = 10 * time.Second
)

// notifier delivers a user-facing macOS notification. The concrete
// implementation is selected by defaultNotifier: a no-op today, swapped for a
// UNUserNotificationCenter-backed notifier (branded banners from the signed
// Bladerunner.app) in a later PR. Kept tiny so the transition machine can be
// unit-tested with a fake.
type notifier interface {
	notify(title, body string)
}

// noopNotifier drops every notification. It is the default until the cgo
// UNUserNotificationCenter bridge lands, and the fallback when running outside
// the signed .app bundle (where UN cannot deliver).
type noopNotifier struct{}

func (noopNotifier) notify(string, string) {}

// defaultNotifier returns the notifier to use for this process. Today it is
// always a no-op; the UN-backed notifier (selected when running inside the
// signed Bladerunner.app) is wired in the notifications-delivery PR.
func defaultNotifier() notifier { return noopNotifier{} }

// splashController shows/hides the "bladerunner is starting…" splash. The
// transition machine drives it (Show on Start, Hide on the first healthy edge)
// without knowing the window implementation, so the cgo NSPopover splash can be
// dropped in later behind the same interface. No-op until then.
type splashController interface {
	Show()
	Hide()
}

// noopSplash is the default splash controller until the cgo splash window lands.
type noopSplash struct{}

func (noopSplash) Show() {}
func (noopSplash) Hide() {}

// defaultSplash returns the splash controller for this process (a no-op until
// the cgo splash window PR).
func defaultSplash() splashController { return noopSplash{} }

// vmNotifier is the edge-triggered notification + splash state machine. It is
// fed every health reading from the poll goroutine (observe) and the Start
// click (onStart) and wake detection (onWake). It is the single place that
// turns a stream of vmState readings into at-most-one banner per real
// transition. Safe for concurrent use: the poll and click goroutines both touch
// it.
//
// The committed state (last) is only ever vmStopped, vmHealthy, or vmWedged.
// vmUnknown is a soft "couldn't read" reading: it never notifies and never
// becomes the committed state — it holds whatever was last known so a transient
// probe failure can't manufacture a false transition.
type vmNotifier struct {
	n      notifier
	splash splashController

	debounceReads      int
	suppressAfterStart time.Duration
	minInterval        time.Duration

	mu             sync.Mutex
	seeded         bool
	last           vmState
	pending        vmState
	pendingCount   int
	expectingStart bool
	lastStartAt    time.Time
	lastNotifyAt   time.Time
}

func newVMNotifier(n notifier, splash splashController) *vmNotifier {
	return &vmNotifier{
		n:                  n,
		splash:             splash,
		debounceReads:      notifyDebounceReads,
		suppressAfterStart: notifySuppressAfterStart,
		minInterval:        notifyMinInterval,
	}
}

// onStart records that the user (or a start policy) just asked the VM to start,
// so wedged/unknown readings are suppressed during boot, and shows the splash.
func (m *vmNotifier) onStart(now time.Time) {
	m.mu.Lock()
	m.expectingStart = true
	m.lastStartAt = now
	m.mu.Unlock()
	m.splash.Show()
}

// onWake emits the one-shot "reconnecting after sleep" banner when the poll loop
// detects the host slept and woke. Rate-limited like any other banner.
func (m *vmNotifier) onWake(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rateLimited(now) {
		return
	}
	m.lastNotifyAt = now
	m.n.notify(notifyTitle, bodyReconnecting)
}

// observe feeds one health reading into the machine, emitting a banner (and
// hiding the splash) only on a real committed transition.
func (m *vmNotifier) observe(st vmState, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// vmUnknown holds: never notify, never commit, and break any wedged streak.
	if st == vmUnknown {
		m.pending = vmUnknown
		m.pendingCount = 0
		return
	}

	if !m.seeded {
		m.last = st
		m.seeded = true
		if st == vmHealthy {
			// Already up at launch (e.g. menubar started at login over a running
			// VM): nothing to announce, just make sure no stale splash lingers.
			m.expectingStart = false
			m.splash.Hide()
		}
		return
	}

	commit, ok := m.commitState(st, now)
	if !ok || commit == m.last {
		return
	}

	prev := m.last
	m.last = commit
	if commit == vmHealthy {
		m.expectingStart = false
		m.splash.Hide()
	}

	body, notify := transitionBody(prev, commit)
	if !notify || m.rateLimited(now) {
		return
	}
	m.lastNotifyAt = now
	m.n.notify(notifyTitle, body)
}

// commitState resolves a reading into the state to commit. healthy/stopped are
// trusted immediately; wedged must repeat debounceReads times and is suppressed
// during the post-start boot window. Returns ok=false when there is nothing to
// commit yet.
func (m *vmNotifier) commitState(st vmState, now time.Time) (vmState, bool) {
	switch st {
	case vmHealthy, vmStopped:
		m.pending = st
		m.pendingCount = 0
		return st, true
	case vmWedged:
		if m.expectingStart && now.Sub(m.lastStartAt) < m.suppressAfterStart {
			return 0, false // booting guest is not "wedged"
		}
		if m.pending == vmWedged {
			m.pendingCount++
		} else {
			m.pending = vmWedged
			m.pendingCount = 1
		}
		if m.pendingCount < m.debounceReads {
			return 0, false
		}
		return vmWedged, true
	default: // vmUnknown handled before commitState
		return 0, false
	}
}

// rateLimited reports whether a banner was sent too recently to send another.
func (m *vmNotifier) rateLimited(now time.Time) bool {
	return !m.lastNotifyAt.IsZero() && now.Sub(m.lastNotifyAt) < m.minInterval
}

// transitionBody maps a committed state transition to a banner body, or
// notify=false when the transition isn't worth announcing.
func transitionBody(prev, cur vmState) (body string, notify bool) {
	switch {
	case cur == vmHealthy && prev == vmStopped:
		return bodyReady, true
	case cur == vmHealthy && prev == vmWedged:
		return bodyRecovered, true
	case cur == vmWedged && prev == vmHealthy:
		return bodyUnresponsive, true
	case cur == vmStopped && (prev == vmHealthy || prev == vmWedged):
		return bodyStopped, true
	default:
		return "", false
	}
}
