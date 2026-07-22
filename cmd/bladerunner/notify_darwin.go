//go:build darwin

package main

import (
	"os"
	"strings"
	"sync"
	"time"

	"github.com/stuffbucket/bladerunner/internal/bootstage"
)

// notifyTitle is the title shown on every banner. The body carries the detail.
const notifyTitle = "Bladerunner"

// Banner bodies, as constants so the machine and its tests reference one source.
const (
	bodyReady         = "Your VM is ready."
	bodyRecovered     = "Your VM recovered and is responding again."
	bodyUnresponsive  = "Your VM is unresponsive — try Restart."
	bodyStopped       = "Your VM stopped."
	bodyReconnecting  = "Woke from sleep — the VM is re-syncing its clock…"
	bodyEngineUpdate  = "An update is ready — choose “Restart VM to finish update”."
	bodyStillStarting = "Still starting… the VM is taking longer than usual."
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
	// notifyBootStuckAfter is how long a single boot stage may sit before we
	// consider the boot "stuck" and surface a conservative one-shot banner. Sized
	// well above a fast boot (which advances through every stage in seconds); the
	// single-flight delay (one confirming poll) means the earliest banner lands at
	// ≈ notifyBootStuckAfter + one poll interval.
	notifyBootStuckAfter = 45 * time.Second
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

// defaultNotifier returns the notifier to use for this process: the branded
// UNUserNotificationCenter notifier when running inside the (signed) .app
// bundle, otherwise a no-op. UN requires a valid bundle id + code signature to
// deliver — a bare `br menubar` from the CLI has neither — so emitting from
// outside the bundle would silently fail (or, accessing the center, raise),
// hence the guard. Only the long-lived menubar process emits; the detached
// `br` subprocesses it spawns are never bundled and must not notify.
func defaultNotifier() notifier {
	if runningInsideAppBundle() {
		return newUNNotifier()
	}
	return noopNotifier{}
}

// runningInsideAppBundle reports whether this process is the menubar binary
// running from inside Bladerunner.app (…/Bladerunner.app/Contents/MacOS/…),
// which is the only context where UNUserNotificationCenter can deliver.
func runningInsideAppBundle() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	return isAppBundlePath(exe)
}

// isAppBundlePath reports whether an executable path lies inside a macOS .app
// bundle's MacOS dir. Split out for testing.
func isAppBundlePath(exe string) bool {
	return strings.Contains(exe, ".app/Contents/MacOS/")
}

// splashController shows/hides the "bladerunner is starting…" splash. The
// transition machine drives it (Show on Start, Hide on the first healthy edge)
// without knowing the window implementation. The real implementation is the cgo
// floating HUD window in ui_bridge_darwin.go (defaultSplash); tests use a fake.
type splashController interface {
	Show()
	Hide()
	// SetStatus updates the splash's phase line (e.g. "Booting Linux…").
	SetStatus(msg string)
}

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
	splashUp       bool // the starting splash is currently shown
	lastStartAt    time.Time
	lastNotifyAt   time.Time

	// Boot-progress tracking, driven by boot-stage transitions (observeBoot).
	// bootStage is the last seen stage ("" = none/terminal); bootStageSince is
	// when it was first seen. bootCandidate queues a single confirming poll so a
	// one-off slow read isn't enough — the boot must still be stuck on the next
	// poll. bootNotified latches the at-most-once "still starting" banner per
	// boot episode.
	bootStage      bootstage.Stage
	bootStageSince time.Time
	bootCandidate  bool
	bootNotified   bool
}

// showSplash/hideSplash track visibility so the splash is shown/hidden at most
// once per episode and a healthy guest can idempotently clear it. Callers hold
// m.mu.
func (m *vmNotifier) showSplash() {
	m.splashUp = true
	m.splash.Show()
}

func (m *vmNotifier) hideSplash() {
	if m.splashUp {
		m.splashUp = false
		m.splash.Hide()
	}
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
	defer m.mu.Unlock()
	m.expectingStart = true
	m.lastStartAt = now
	m.resetBootLocked() // re-arm boot-progress tracking for this fresh start
	m.showSplash()
}

// onPresent handles a second-launch handoff. It re-surfaces the starting splash
// ONLY while a start is actually in progress; showing the "starting" splash over
// an already-running VM would strand it on screen (no transition is coming to
// hide it). When idle/healthy a second launch is a quiet no-op.
func (m *vmNotifier) onPresent() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.expectingStart {
		m.showSplash()
	}
}

// notifyEngineUpdate posts the one-shot "engine update ready" banner when the
// running VM is older than this menubar. The poll loop already gates it to once
// per session, so this just emits.
func (m *vmNotifier) notifyEngineUpdate() {
	m.n.notify(notifyTitle, bodyEngineUpdate)
}

// onWake emits the one-shot "woke from sleep" banner when the poll loop detects
// the host slept and woke. The guest watchdog does the actual re-sync; this only
// tells the user why the VM may be briefly out of sync. Rate-limited like any
// other banner.
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

	// A healthy guest must always clear the starting splash, regardless of how it
	// was shown (Start click, start policy, or a second-launch handoff) or
	// whether this is a real transition — otherwise a splash shown over an
	// already-running VM would be stranded. Idempotent via the splashUp guard.
	if st == vmHealthy {
		m.expectingStart = false
		m.splash.SetStatus("Ready") // flashes before the min-visible hide
		m.hideSplash()
	}

	// A start that stops before ever reaching healthy (aborted boot, user hit
	// Stop, VZ failed) must also clear the splash — otherwise it strands on
	// screen until the multi-minute safety timeout. hideSplash is idempotent.
	if st == vmStopped {
		m.expectingStart = false
		m.resetBootLocked() // an aborted boot must not leak a queued candidate
		m.hideSplash()
	}

	// vmUnknown holds: never notify, never commit, and break any wedged streak.
	if st == vmUnknown {
		m.pending = vmUnknown
		m.pendingCount = 0
		return
	}

	if !m.seeded {
		m.last = st
		m.seeded = true
		return
	}

	commit, ok := m.commitState(st, now)
	if !ok || commit == m.last {
		return
	}

	prev := m.last
	m.last = commit

	body, notify := transitionBody(prev, commit)
	if !notify || m.rateLimited(now) {
		return
	}
	m.lastNotifyAt = now
	m.n.notify(notifyTitle, body)
}

// observeBoot feeds one boot-stage reading (from the boot-stage.json the running
// `br start` publishes) into the boot-progress machine. It is edge-triggered like
// observe: a stage that sits past notifyBootStuckAfter surfaces AT MOST ONE
// conservative "still starting…" banner per boot episode, and any forward
// progress cancels a queued banner (coalesce). Fast boots — which advance through
// every stage in seconds — never dwell long enough to notify.
func (m *vmNotifier) observeBoot(stage bootstage.Stage, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// No live boot, or a terminal stage: not "stuck starting". Re-arm for the
	// next episode.
	if stage == "" || stage == bootstage.Ready || stage == bootstage.Failed {
		m.resetBootLocked()
		return
	}

	// Advanced to a new stage (or the first stage of this episode): restart the
	// dwell clock and drop any queued candidate — progress coalesces.
	if stage != m.bootStage {
		m.bootStage = stage
		m.bootStageSince = now
		m.bootCandidate = false
		return
	}

	// At most one boot-progress banner per episode.
	if m.bootNotified {
		return
	}

	// Same stage as last poll: has it dwelt past the stuck threshold?
	if now.Sub(m.bootStageSince) < notifyBootStuckAfter {
		return
	}
	// Single-flight: the first stuck read only queues; require one more stuck
	// poll before firing, so a lone slow reading can't trip a false banner.
	if !m.bootCandidate {
		m.bootCandidate = true
		return
	}
	if m.rateLimited(now) {
		return
	}
	m.bootCandidate = false
	m.bootNotified = true
	m.lastNotifyAt = now
	m.n.notify(notifyTitle, bodyStillStarting)
}

// resetBootLocked clears the boot-progress tracking so the next boot episode
// starts fresh. Caller holds m.mu.
func (m *vmNotifier) resetBootLocked() {
	m.bootStage = ""
	m.bootStageSince = time.Time{}
	m.bootCandidate = false
	m.bootNotified = false
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
