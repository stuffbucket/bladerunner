//go:build darwin

package main

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"syscall"
	"time"

	"fyne.io/systray"

	"github.com/stuffbucket/bladerunner/internal/bootstage"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/control"
)

const menubarRefreshInterval = 3 * time.Second

// startActionTimeout bounds how long a StartOnFirstAction click waits for the
// lazily-started guest to become healthy before giving up on running the
// deferred action (matches the splash auto-dismiss budget for a cold boot).
const startActionTimeout = 5 * time.Minute

// wakeGapSeconds: if wall-clock advances far more than the poll interval between
// two polls, the Mac slept and woke (the agent was frozen meanwhile). On wake we
// surface a banner; the guest watchdog heals the clock + relays autonomously.
const wakeGapSeconds = 60

// status-icon rendering constants.
const (
	iconSize    = 22
	alphaOpaque = 0xFF
	// alphaShift converts a 16-bit alpha sample (from color.Color's RGBA()) to 8-bit.
	alphaShift = 8
	// glyph colors: gray (stopped), green (running+healthy), amber (running but
	// the guest is not answering — "wedged" — or status unknown).
	grayR, grayG, grayB    = 0x9A, 0xA4, 0xA8
	greenR, greenG, greenB = 0x27, 0xC9, 0x3F
	amberR, amberG, amberB = 0xF0, 0x97, 0x3A
)

// menubarGlyphPNG is the bladerunner "b" mark (44x44, black on transparent).
// Its per-pixel alpha is the glyph coverage; statusIcon tints that alpha by VM
// state. Regenerate from assets/brand/menubar-b.svg via scripts/gen-brand-assets.sh.
//
//go:embed assets/menubar-b.png
var menubarGlyphPNG []byte

// menubarGlyph is menubarGlyphPNG decoded once at startup (used as an alpha mask).
var menubarGlyph = mustDecodePNG(menubarGlyphPNG)

func mustDecodePNG(b []byte) image.Image {
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		panic("decode embedded menubar glyph: " + err.Error())
	}
	return img
}

// vmState is the menubar's view of the VM: not just up/down, but whether the
// guest actually answers a liveness probe (so a wedged guest is distinguishable
// from a healthy one).
type vmState int

const (
	vmStopped vmState = iota // host process not running
	vmHealthy                // running and the guest answers the probe
	vmWedged                 // host alive but guest unresponsive (the failure mode that breaks web/shell)
	vmUnknown                // running but status could not be read
	// vmAmbiguous means several instances are running and none was selected,
	// so the menubar has no single VM to report on and must not act on one.
	vmAmbiguous
)

// vmReading is one health sample together with the instance it describes. The
// two travel together so the status row can never name one VM while the icon
// reports another.
type vmReading struct {
	state  vmState
	target menubarTarget
}

// readVM resolves the instance the menubar reports on and probes it.
func readVM() vmReading {
	target := currentMenubarTarget()
	if target.ambiguous {
		return vmReading{state: vmAmbiguous, target: target}
	}
	return vmReading{state: vmHealthAt(target.stateDir), target: target}
}

func runMenubar() error {
	// Enforce a version-aware single-instance rule BEFORE systray.Run seizes the
	// main thread. A second launch of the same-or-older version hands off to the
	// running instance and exits 0 — quietly, never an error, so the KeepAlive
	// LaunchAgent does not relaunch-fight it. A NEWER launch (an upgrade) instead
	// asks the running instance to step down (systray.Quit) and takes over; the
	// VM runs in a detached `br start` and is untouched, so containers keep
	// running.
	release, already := acquireMenubarLock(version, firePresent, func() { systray.Quit() })
	if already {
		return nil
	}
	defer release()

	// systray.Run takes over the main thread and blocks until Quit.
	systray.Run(onMenubarReady, func() {})
	return nil
}

// menubarItems is the fixed set of status-bar rows, captured once at build time
// so the state mapping and the click loop can address them by name instead of
// passing ten *systray.MenuItem values around. The rows never change identity
// for the life of the process; only their title and enabled state do.
type menubarItems struct {
	status    *systray.MenuItem
	update    *systray.MenuItem
	start     *systray.MenuItem
	stop      *systray.MenuItem
	reconnect *systray.MenuItem
	restart   *systray.MenuItem
	web       *systray.MenuItem
	shell     *systray.MenuItem
	settings  *systray.MenuItem
	quit      *systray.MenuItem
}

// buildMenubarItems lays out the status-bar menu in display order and returns
// the rows. It must run on the systray callback (i.e. from onMenubarReady);
// systray has no menu to attach to before that.
func buildMenubarItems() *menubarItems {
	m := &menubarItems{}
	m.status = systray.AddMenuItem("Checking…", "Bladerunner VM status")
	m.status.Disable()
	systray.AddSeparator()
	// Shown only when the running VM (engine) was started by an older build than
	// this menubar — a user-gated, non-destructive "restart to apply" (Docker
	// Desktop's app-vs-engine split). Hidden until detected.
	m.update = systray.AddMenuItem("Restart VM to finish update", "Gracefully restart the VM to apply the new bladerunner version")
	m.update.Hide()
	m.start = systray.AddMenuItem("Start VM", "Boot the bladerunner VM")
	m.stop = systray.AddMenuItem("Stop VM", "Gracefully stop the VM")
	m.reconnect = systray.AddMenuItem("Reconnect", "Re-sync the guest after sleep (clock + forwarders) without restarting")
	m.restart = systray.AddMenuItem("Restart VM", "Stop and start the VM (fixes a wedged/unresponsive guest)")
	systray.AddSeparator()
	m.web = systray.AddMenuItem("Open Web UI…", "Open the Incus web UI with single sign-on")
	m.shell = systray.AddMenuItem("Open Shell…", "Open a Terminal shell inside the VM")
	systray.AddSeparator()
	m.settings = systray.AddMenuItem("Settings…", "Edit bladerunner settings")
	m.quit = systray.AddMenuItem("Quit", "Quit the bladerunner menubar")
	return m
}

// menubarStartPolicy reads the host-wide start policy that governs whether the
// menubar auto-starts the VM at launch (StartOnLaunch) or lazily on the first
// Web/Shell action (StartOnFirstAction). A missing or unreadable settings file
// falls back to StartManual, today's behavior, so a broken file never
// surprises the user with an unasked-for boot.
func menubarStartPolicy() config.StartPolicy {
	if s, err := config.LoadSettings(menubarSettingsDir()); err == nil {
		return s.StartPolicy
	}
	return config.StartManual
}

// statusTitle maps a VM state to the disabled first row of the menu. phase and
// booting come from bootingPhase and are consulted only when the guest is not
// answering: while a boot is in progress we surface the live, friendly phase
// ("Booting Linux…", "Starting Incus…") instead of a scary "unresponsive" — the
// guest simply isn't answering yet. A stale or absent boot-stage file (booting
// false) means it is a genuine wedge and we say so.
func statusTitle(st vmState, phase string, booting bool, target menubarTarget) string {
	if st == vmAmbiguous {
		return "Several VMs running — choose one with 'br instances'"
	}
	return qualify(baseStatusTitle(st, phase, booting), target)
}

// baseStatusTitle is the status wording for a single resolved instance.
func baseStatusTitle(st vmState, phase string, booting bool) string {
	switch st {
	case vmStopped:
		return "Stopped"
	case vmHealthy:
		return "Running — healthy"
	case vmWedged, vmUnknown:
		if booting {
			return phase
		}
		if st == vmWedged {
			return "Running — not responding"
		}
		return "Running — status unknown"
	default:
		return "Running — status unknown"
	}
}

// qualify names the instance the status describes, unless it is the flat
// default — the single-VM install reads exactly as it always did, and only a
// user who actually has a named instance up pays for the extra words.
func qualify(title string, target menubarTarget) string {
	if target.isDefault || target.name == "" || target.name == config.DefaultInstanceName {
		return title
	}
	return title + " (" + target.name + ")"
}

// menuEnablement is which action rows are clickable for a given VM state. It is
// split out from the menu itself so the policy can be asserted in a test
// without a live status bar.
type menuEnablement struct {
	start     bool
	stop      bool
	reconnect bool
	restart   bool
	web       bool
	shell     bool
}

// enablementFor decides which actions make sense in state st. Stop, Reconnect
// and Restart need a running VM (Reconnect is the light heal after sleep;
// Restart is the fix when the guest is fully wedged), Start needs a stopped
// one, and Web/Shell normally need a healthy guest — except under
// StartOnFirstAction, where they stay clickable while stopped so that a click
// can lazily boot the VM.
func enablementFor(st vmState, firstAction bool) menuEnablement {
	// Every row shells out to `br <verb>` with no --instance, and `br` refuses
	// to guess among several running VMs. Presenting the rows as clickable
	// would promise an action that cannot happen.
	if st == vmAmbiguous {
		return menuEnablement{}
	}
	running := st != vmStopped
	return menuEnablement{
		start:     st == vmStopped,
		stop:      running,
		reconnect: running,
		restart:   running,
		web:       webShellEnabled(st, firstAction),
		shell:     webShellEnabled(st, firstAction),
	}
}

// apply pushes a health reading onto the menu: icon tint, status row, and which
// actions are clickable. The boot-stage file is read only when the guest is not
// answering, so the steady-state poll stays free of disk I/O.
func (m *menubarItems) apply(r vmReading, firstAction bool) {
	st := r.state
	systray.SetIcon(statusIcon(st))

	phase, booting := "", false
	if st == vmWedged || st == vmUnknown {
		phase, booting = bootingPhase(r.target.stateDir)
	}
	m.status.SetTitle(statusTitle(st, phase, booting, r.target))

	en := enablementFor(st, firstAction)
	setEnabled(m.start, en.start)
	setEnabled(m.stop, en.stop)
	setEnabled(m.reconnect, en.reconnect)
	setEnabled(m.restart, en.restart)
	setEnabled(m.web, en.web)
	setEnabled(m.shell, en.shell)
}

// triggerStart boots the VM: show the splash and arm the notify machine, then
// launch `br start` detached. This is the single canonical start path, shared
// by the Start click, StartOnLaunch and the lazy StartOnFirstAction boot.
// (`br start`'s control socket refuses a second bind, so a racing auto-start
// plus a manual start can never double-boot.)
func (m *menubarItems) triggerStart(notif *vmNotifier) {
	m.status.SetTitle("bladerunner: starting…")
	notif.onStart(time.Now())
	_ = launchDetached("start")
}

// runWhenHealthy polls until the guest answers, then runs action. Used by
// StartOnFirstAction to perform the Web/Shell action once the lazily-started VM
// is ready. It gives up silently at startActionTimeout rather than running the
// action against a guest that never came up.
func runWhenHealthy(action func()) {
	deadline := time.Now().Add(startActionTimeout)
	for time.Now().Before(deadline) {
		if vmHealth() == vmHealthy {
			action()
			return
		}
		time.Sleep(menubarRefreshInterval)
	}
}

// runOrLazyStart runs a Web/Shell action now, or — under StartOnFirstAction
// with the VM stopped — boots the VM first and runs the action once the guest
// answers. The deferred case runs on its own goroutine so the click loop is
// never blocked for the length of a cold boot.
func (m *menubarItems) runOrLazyStart(notif *vmNotifier, firstAction bool, action func()) {
	if firstAction && vmHealth() == vmStopped {
		m.triggerStart(notif)
		go runWhenHealthy(action)
		return
	}
	action()
}

// hostWokeFromSleep reports whether the wall clock jumped much further between
// two polls than the poll interval can account for — the signature of the Mac
// sleeping and waking with the agent frozen in between. A stopped VM is never
// reported: there is no guest whose clock and forwarders need resyncing.
func hostWokeFromSleep(st vmState, prevWall, nowWall int64) bool {
	// No single guest whose clock and forwarders need resyncing: stopped means
	// there is none, ambiguous means we do not know which.
	if st == vmStopped || st == vmAmbiguous {
		return false
	}
	return nowWall-prevWall > int64(menubarRefreshInterval/time.Second)+wakeGapSeconds
}

// offerLatest hands v to ch without ever blocking. The channels it feeds hold a
// single slot; if that slot is still full the menu goroutine has not caught up
// and this reading is dropped rather than stalling the poll loop.
func offerLatest[T any](ch chan<- T, v T) {
	select {
	case ch <- v:
	default:
	}
}

// pollVMHealth samples VM health forever, off the click loop, so that a slow
// probe (a wedged guest) never blocks the menu. It feeds every reading to the
// transition machine — not just the ones that fit in healthCh — so edge
// detection never misses a change, publishes the latest reading on healthCh,
// and signals updateCh once if the running engine turns out to be older than
// this menubar. It also detects host sleep/wake and surfaces a banner; the
// guest watchdog owns the actual post-sleep recovery.
func pollVMHealth(notif *vmNotifier, healthCh chan<- vmReading, updateCh chan<- struct{}) {
	lastWall := time.Now().Unix()
	engineChecked := false
	for {
		reading := readVM()
		st := reading.state
		now := time.Now().Unix()
		if hostWokeFromSleep(st, lastWall, now) {
			notif.onWake(time.Now()) // banner only; the guest watchdog self-heals
		}
		lastWall = now
		notif.observe(st, time.Now())
		// Once the guest is up, check whether it's running an OLDER engine than
		// this (possibly just-upgraded) menubar; if so, surface a user-gated
		// "restart to apply". Checked once per session.
		if st == vmHealthy && !engineChecked {
			engineChecked = true
			if engineUpgradeAvailable(version, reading.target.stateDir) {
				notif.notifyEngineUpdate()
				offerLatest(updateCh, struct{}{})
			}
		}
		offerLatest(healthCh, reading)
		time.Sleep(menubarRefreshInterval)
	}
}

// dispatchClicks is the menu's event loop: it folds health readings onto the
// menu and runs the action behind each row. Long-running commands are launched
// on their own goroutine (or detached entirely) so a click is never able to
// wedge the loop. It returns only on Quit, which tears the process down.
func (m *menubarItems) dispatchClicks(notif *vmNotifier, healthCh <-chan vmReading, updateCh <-chan struct{}, firstAction bool) {
	for {
		select {
		case reading := <-healthCh:
			m.apply(reading, firstAction)
		case <-updateCh:
			m.update.Show() // an older engine is running; offer the restart
		case <-m.update.ClickedCh:
			m.status.SetTitle("bladerunner: updating…")
			m.update.Hide()
			go runnerRun("upgrade") // graceful save/restore to the new engine
		case <-m.start.ClickedCh:
			m.triggerStart(notif)
		case <-m.stop.ClickedCh:
			m.status.SetTitle("bladerunner: stopping…")
			go runnerRun("stop")
		case <-m.reconnect.ClickedCh:
			m.status.SetTitle("bladerunner: reconnecting…")
			go runnerRun("reconnect")
		case <-m.restart.ClickedCh:
			m.status.SetTitle("bladerunner: restarting…")
			go restartVM()
		case <-m.web.ClickedCh:
			m.runOrLazyStart(notif, firstAction, func() { _ = launchDetached("web") })
		case <-m.shell.ClickedCh:
			m.runOrLazyStart(notif, firstAction, openShellTerminal)
		case <-m.settings.ClickedCh:
			showSettingsWindow()
		case <-m.quit.ClickedCh:
			systray.Quit()
			return
		}
	}
}

// onMenubarReady is systray's ready callback: it builds the menu, resolves the
// start policy, wires up notifications and the cartridge watcher, honors
// StartOnLaunch, and hands the running system over to the poll loop and the
// click loop. It returns immediately; systray owns the main thread from here.
func onMenubarReady() {
	systray.SetIcon(statusIcon(vmStopped))
	systray.SetTooltip("bladerunner")

	items := buildMenubarItems()

	startPolicy := menubarStartPolicy()
	firstAction := startPolicy == config.StartOnFirstAction
	items.apply(vmReading{state: vmStopped}, firstAction)

	// notif turns the stream of health readings + Start clicks into at-most-one
	// native banner per real VM transition, and drives the starting splash.
	splash := defaultSplash()
	notifications := defaultNotifier()
	notif := newVMNotifier(notifications, splash)

	// Goal 4: notice a cartridge DMG being mounted and offer to boot it. The
	// watcher owns its own DiskArbitration session and delivers through the
	// notifier above, so an insertion is announced the same way every other VM
	// event is. It runs for the life of the menubar; systray.Quit takes the
	// process with it, so the stop function is deliberately dropped.
	_ = watchCartridgesForMenubar(newCartridgePrompter(notifications))

	// When a second launch hands off (see acquireMenubarLock), re-surface the
	// splash only if a start is in progress — never strand a "starting" splash
	// over an already-running VM.
	setMenubarPresentHandler(notif.onPresent)

	// StartOnLaunch: boot the VM now if it isn't already up.
	if startPolicy == config.StartOnLaunch && vmHealth() == vmStopped {
		items.triggerStart(notif)
	}

	healthCh := make(chan vmReading, 1)
	updateCh := make(chan struct{}, 1)
	go pollVMHealth(notif, healthCh, updateCh)
	go items.dispatchClicks(notif, healthCh, updateCh, firstAction)
}

// engineUpgradeAvailable reports whether the running VM (started by some prior
// `br start`) is an OLDER build than this menubar (menubarVer) — i.e. a restart
// would move the engine up to the current version. Returns false when the VM
// isn't reachable or versions don't compare (dev/unknown), so it never nags
// spuriously.
func engineUpgradeAvailable(menubarVer, stateDir string) bool {
	serverVer, err := control.NewClient(stateDir).ServerVersion()
	if err != nil {
		return false
	}
	return shouldStepDown(menubarVer, serverVer)
}

// webShellEnabled reports whether the Web/Shell menu items should be clickable
// for a given VM state. They need a healthy guest, except under
// StartOnFirstAction where they stay clickable while stopped so a click can
// lazily boot the VM.
func webShellEnabled(st vmState, firstAction bool) bool {
	return st == vmHealthy || (firstAction && st == vmStopped)
}

func setEnabled(m *systray.MenuItem, enabled bool) {
	if enabled {
		m.Enable()
	} else {
		m.Disable()
	}
}

// vmHealth probes the VM: stopped (no host process), healthy (guest answers the
// liveness probe), wedged (host alive but guest unresponsive), or unknown. The
// probe itself runs server-side in the VM process; this is a cheap socket call.
func vmHealth() vmState { return readVM().state }

// vmHealthAt probes one instance by its state dir.
func vmHealthAt(stateDir string) vmState {
	c := control.NewClient(stateDir)
	if !c.IsRunning() {
		return vmStopped
	}
	status, err := c.GetStatus()
	if err != nil {
		return vmUnknown
	}
	switch status {
	case control.StatusRunning:
		return vmHealthy
	case control.StatusUnreachable:
		return vmWedged
	case control.StatusStopped:
		return vmStopped
	default:
		return vmUnknown
	}
}

// bootingPhase returns the live, human-friendly boot phase (e.g. "Starting
// Incus…") while a boot is in progress. The phase is published by whichever
// process owns the booting VM — internal/vmhost's boot-stage publisher, which
// normally runs in the holder rather than in any `br start` this menubar
// launched — and reaches here only through the state file. ok is false when
// there is no recent boot-stage file, meaning either no boot is underway or it
// finished, so callers fall back to the steady-state label.
func bootingPhase(stateDir string) (string, bool) {
	s, ok := bootstage.Read(stateDir)
	if !ok || time.Since(s.UpdatedAt) > 90*time.Second {
		return "", false
	}
	return bootstage.Message(s.Stage), true
}

// restartVM stops the VM (graceful, forcing after a timeout) then starts a fresh
// one — the fix for a wedged guest.
func restartVM() {
	runnerRun("stop")
	_ = launchDetached("start")
}

// runnerSelf returns the path to this binary so menu actions invoke the same
// 'runner' the user installed.
func runnerSelf() string {
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return "br"
}

// launchDetached starts `br <args...>` detached (new session) so a
// long-running command (start, web) keeps running after the menu action returns.
func launchDetached(args ...string) error {
	cmd := exec.CommandContext(context.Background(), runnerSelf(), args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// Detached: the process should outlive this short-lived context.
	cmd.Cancel = func() error { return nil }
	return cmd.Start()
}

// runnerRun runs `br <args...>` to completion (for short commands like stop).
func runnerRun(args ...string) {
	_ = exec.CommandContext(context.Background(), runnerSelf(), args...).Run()
}

// openShellTerminal opens Terminal.app running `br shell`.
func openShellTerminal() {
	script := fmt.Sprintf("tell application \"Terminal\"\n  do script \"%s shell\"\n  activate\nend tell", runnerSelf())
	_ = exec.CommandContext(context.Background(), "osascript", "-e", script).Start()
}

// statusIcon renders the bladerunner "b" mark tinted by VM state: gray
// (stopped), green (running+healthy), amber (wedged/unknown). The embedded
// glyph is an alpha mask; we composite the state color through its coverage and
// box-average the 2x mask down to the status-item size so the letter edges stay
// crisp on Retina menu bars.
func statusIcon(state vmState) []byte {
	dot := color.RGBA{R: grayR, G: grayG, B: grayB, A: alphaOpaque}
	switch state {
	case vmHealthy:
		dot = color.RGBA{R: greenR, G: greenG, B: greenB, A: alphaOpaque}
	case vmWedged, vmUnknown, vmAmbiguous:
		dot = color.RGBA{R: amberR, G: amberG, B: amberB, A: alphaOpaque}
	case vmStopped:
		// gray (default)
	}

	img := image.NewNRGBA(image.Rect(0, 0, iconSize, iconSize))
	sb := menubarGlyph.Bounds()
	sx := float64(sb.Dx()) / float64(iconSize)
	sy := float64(sb.Dy()) / float64(iconSize)
	for y := range iconSize {
		for x := range iconSize {
			x0, x1 := sb.Min.X+int(float64(x)*sx), sb.Min.X+int(float64(x+1)*sx)
			y0, y1 := sb.Min.Y+int(float64(y)*sy), sb.Min.Y+int(float64(y+1)*sy)
			if x1 <= x0 {
				x1 = x0 + 1
			}
			if y1 <= y0 {
				y1 = y0 + 1
			}
			var sum, n uint32
			for yy := y0; yy < y1; yy++ {
				for xx := x0; xx < x1; xx++ {
					_, _, _, a := menubarGlyph.At(xx, yy).RGBA()
					sum += a >> alphaShift // 16-bit -> 8-bit alpha
					n++
				}
			}
			img.SetNRGBA(x, y, color.NRGBA{R: dot.R, G: dot.G, B: dot.B, A: uint8(sum / n)})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
