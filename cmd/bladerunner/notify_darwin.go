//go:build darwin

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
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
	bodyReconnecting = "Woke from sleep — the VM is re-syncing its clock…"
	bodyEngineUpdate = "An update is ready — choose “Restart VM to finish update”."
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

// --- cartridge insertion --------------------------------------------------
//
// Goal 4 needs something the banner machinery above does not do: a QUESTION.
// A banner is one-way, and "a cartridge appeared, shall I boot it?" cannot be
// answered by one. So the notifier is reused verbatim for the two one-way
// messages (a cartridge that cannot be booted, and a boot that has started) and
// the question itself is a native dialog — the same osascript route the menubar
// already uses to open Terminal.

// Copy for the cartridge messages, kept beside the other bodies so one file
// holds everything the menubar says.
const (
	bodyCartridgeUnbootableFmt = "Cartridge “%s” cannot be booted: %s"
	bodyCartridgeBootingFmt    = "Booting cartridge “%s”…"
	promptCartridgeFmt         = "Cartridge “%s” was inserted.\n\nBoot it now?\n\n%s"
)

// Dialog tuning. The dialog dismisses itself so an unattended Mac is not left
// with a modal on screen forever, and the exec budget is comfortably larger
// than that so the timeout is the dialog's decision and not a killed process.
const (
	cartridgeDialogGiveUpSecs = 120
	cartridgeDialogBudget     = 5 * time.Minute
	// cartridgeBootButton is the accept button's title; the reply is matched
	// against it, so the two must not drift.
	cartridgeBootButton    = "Boot"
	cartridgeDeclineButton = "Not now"
)

// cartridgePrompter is what the mount watcher needs from the UI: two
// announcements and one question. It is an interface so the watcher can be
// driven by a fake, and so the menubar's notifier stays the only thing that
// knows how a message is delivered.
type cartridgePrompter interface {
	// warnCartridge reports a cartridge that was inserted but cannot be booted.
	warnCartridge(name, reason string)
	// confirmBootCartridge asks whether to boot a bootable cartridge. It blocks
	// until the user answers, so callers must not run it on a callback queue.
	confirmBootCartridge(name, source string) bool
	// announceCartridgeBoot reports that a holder is starting.
	announceCartridgeBoot(name string)
}

// notifierPrompter implements cartridgePrompter over the menubar's existing
// notifier plus a native dialog for the question.
type notifierPrompter struct {
	n notifier
	// ask is the dialog, injectable so the wiring can be exercised without
	// putting a modal on screen. Nil means the real one.
	ask func(name, source string) bool
}

// newCartridgePrompter wraps the menubar's notifier as a cartridge prompter.
func newCartridgePrompter(n notifier) cartridgePrompter {
	return notifierPrompter{n: n}
}

func (p notifierPrompter) warnCartridge(name, reason string) {
	p.n.notify(notifyTitle, fmt.Sprintf(bodyCartridgeUnbootableFmt, name, reason))
}

func (p notifierPrompter) announceCartridgeBoot(name string) {
	p.n.notify(notifyTitle, fmt.Sprintf(bodyCartridgeBootingFmt, name))
}

func (p notifierPrompter) confirmBootCartridge(name, source string) bool {
	if p.ask != nil {
		return p.ask(name, source)
	}
	return askBootCartridge(name, source)
}

// askBootCartridge shows the native "boot this cartridge?" dialog and reports
// whether the user accepted.
//
// Anything other than an explicit click on the accept button is a decline:
// pressing Escape (the cancel button) exits non-zero, and the self-dismiss
// returns an empty button. Defaulting to "no" matters because booting a
// cartridge commits a whole VM's worth of RAM.
func askBootCartridge(name, source string) bool {
	script := fmt.Sprintf(
		"display dialog %s with title %s buttons {%s, %s} default button %s cancel button %s with icon note giving up after %d",
		appleScriptString(fmt.Sprintf(promptCartridgeFmt, name, source)),
		appleScriptString(notifyTitle),
		appleScriptString(cartridgeDeclineButton),
		appleScriptString(cartridgeBootButton),
		appleScriptString(cartridgeBootButton),
		appleScriptString(cartridgeDeclineButton),
		cartridgeDialogGiveUpSecs,
	)
	ctx, cancel := context.WithTimeout(context.Background(), cartridgeDialogBudget)
	defer cancel()
	out, err := exec.CommandContext(ctx, "osascript", "-e", script).Output()
	if err != nil {
		return false // cancel button, or osascript could not run at all
	}
	return strings.Contains(string(out), "button returned:"+cartridgeBootButton)
}

// appleScriptString renders s as an AppleScript string literal. Cartridge names
// and paths come off a mounted volume, i.e. from outside this machine, so they
// are never pasted into the script unquoted.
func appleScriptString(s string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
	return `"` + escaped + `"`
}
