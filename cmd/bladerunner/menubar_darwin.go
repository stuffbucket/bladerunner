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
// auto-reconnect to heal the guest's clock + vsock forwarders.
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
	amberR, amberG, amberB = 0xFF, 0xBD, 0x2E
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
)

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

//nolint:gocyclo // onMenubarReady is a setup+dispatch function — menu build, the apply() state mapping, and the click select; the start-policy branches tip it past the ceiling without adding real branching complexity.
func onMenubarReady() {
	systray.SetIcon(statusIcon(vmStopped))
	systray.SetTooltip("bladerunner")

	mStatus := systray.AddMenuItem("Checking…", "Bladerunner VM status")
	mStatus.Disable()
	systray.AddSeparator()
	// Shown only when the running VM (engine) was started by an older build than
	// this menubar — a user-gated, non-destructive "restart to apply" (Docker
	// Desktop's app-vs-engine split). Hidden until detected.
	mUpdate := systray.AddMenuItem("Restart VM to finish update", "Gracefully restart the VM to apply the new bladerunner version")
	mUpdate.Hide()
	mStart := systray.AddMenuItem("Start VM", "Boot the bladerunner VM")
	mStop := systray.AddMenuItem("Stop VM", "Gracefully stop the VM")
	mReconnect := systray.AddMenuItem("Reconnect", "Re-sync the guest after sleep (clock + forwarders) without restarting")
	mRestart := systray.AddMenuItem("Restart VM", "Stop and start the VM (fixes a wedged/unresponsive guest)")
	systray.AddSeparator()
	mWeb := systray.AddMenuItem("Open Web UI…", "Open the Incus web UI with single sign-on")
	mShell := systray.AddMenuItem("Open Shell…", "Open a Terminal shell inside the VM")
	systray.AddSeparator()
	mSettings := systray.AddMenuItem("Settings…", "Edit bladerunner settings")
	mQuit := systray.AddMenuItem("Quit", "Quit the bladerunner menubar")

	// Read the host-wide start policy once. It governs whether the menubar
	// auto-starts the VM at launch (StartOnLaunch) or lazily on the first
	// Web/Shell action (StartOnFirstAction). Default StartManual is today's
	// behavior; a missing/invalid settings file falls back to it.
	startPolicy := config.StartManual
	if s, err := config.LoadSettings(config.DefaultStateDir()); err == nil {
		startPolicy = s.StartPolicy
	}
	firstAction := startPolicy == config.StartOnFirstAction

	apply := func(st vmState) {
		systray.SetIcon(statusIcon(st))
		switch st {
		case vmStopped:
			mStatus.SetTitle("bladerunner: stopped")
		case vmHealthy:
			mStatus.SetTitle("bladerunner: running")
		case vmWedged:
			mStatus.SetTitle("bladerunner: running (unresponsive — try Restart)")
		case vmUnknown:
			mStatus.SetTitle("bladerunner: running (status unknown)")
		}
		running := st != vmStopped
		setEnabled(mStart, st == vmStopped)
		setEnabled(mStop, running)
		setEnabled(mReconnect, running) // light heal after sleep
		setEnabled(mRestart, running)   // restart is the fix when fully wedged
		// Web/Shell normally require a healthy guest. Under StartOnFirstAction
		// they stay clickable while stopped so a click can lazily boot the VM.
		setEnabled(mWeb, webShellEnabled(st, firstAction))
		setEnabled(mShell, webShellEnabled(st, firstAction))
	}
	apply(vmStopped)

	// notif turns the stream of health readings + Start clicks into at-most-one
	// native banner per real VM transition, and drives the starting splash.
	// Today both the notifier and splash are no-ops; the cgo UNUserNotification
	// and NSPopover implementations are dropped in behind these interfaces in
	// later PRs.
	splash := defaultSplash()
	notif := newVMNotifier(defaultNotifier(), splash)

	// When a second launch hands off (see acquireMenubarLock), re-surface the
	// splash only if a start is in progress — never strand a "starting" splash
	// over an already-running VM.
	setMenubarPresentHandler(notif.onPresent)

	// triggerStart boots the VM the same way the Start item does — show the
	// splash + arm the notify machine, then launch `br start` detached. The
	// single canonical start path, reused by the Start click and the policies.
	// (`br start`'s control socket refuses a second bind, so a racing auto-start
	// + manual start can never double-boot.)
	triggerStart := func() {
		mStatus.SetTitle("bladerunner: starting…")
		notif.onStart(time.Now())
		_ = launchDetached("start")
	}

	// runWhenHealthy polls until the guest answers (bounded) then runs action —
	// used by StartOnFirstAction to perform the Web/Shell action once the
	// lazily-started VM is ready.
	runWhenHealthy := func(action func()) {
		deadline := time.Now().Add(startActionTimeout)
		for time.Now().Before(deadline) {
			if vmHealth() == vmHealthy {
				action()
				return
			}
			time.Sleep(menubarRefreshInterval)
		}
	}

	// StartOnLaunch: boot the VM now if it isn't already up.
	if startPolicy == config.StartOnLaunch && vmHealth() == vmStopped {
		triggerStart()
	}

	// Poll health off the click loop so a slow probe (a wedged guest) never
	// blocks the menu. The same loop detects host sleep/wake (a big wall-clock
	// jump between polls) and auto-reconnects to heal the guest.
	healthCh := make(chan vmState, 1)
	updateCh := make(chan struct{}, 1)
	go func() {
		lastWall := time.Now().Unix()
		engineChecked := false
		for {
			st := vmHealth()
			now := time.Now().Unix()
			if st != vmStopped && now-lastWall > int64(menubarRefreshInterval/time.Second)+wakeGapSeconds {
				notif.onWake(time.Now())
				go runnerRun("reconnect") // self-heal after a detected wake
			}
			lastWall = now
			// Feed every reading (not just the ones that fit in the channel) to
			// the transition machine, so edge detection never misses a change.
			notif.observe(st, time.Now())
			// Once the guest is up, check whether it's running an OLDER engine
			// than this (possibly just-upgraded) menubar; if so, surface a
			// user-gated "restart to apply". Checked once per session.
			if st == vmHealthy && !engineChecked {
				engineChecked = true
				if engineUpgradeAvailable(version) {
					notif.notifyEngineUpdate()
					select {
					case updateCh <- struct{}{}:
					default:
					}
				}
			}
			select {
			case healthCh <- st:
			default:
			}
			time.Sleep(menubarRefreshInterval)
		}
	}()

	go func() {
		for {
			select {
			case st := <-healthCh:
				apply(st)
			case <-updateCh:
				mUpdate.Show() // an older engine is running; offer the restart
			case <-mUpdate.ClickedCh:
				mStatus.SetTitle("bladerunner: updating…")
				mUpdate.Hide()
				go runnerRun("upgrade") // graceful save/restore to the new engine
			case <-mStart.ClickedCh:
				triggerStart()
			case <-mStop.ClickedCh:
				mStatus.SetTitle("bladerunner: stopping…")
				go runnerRun("stop")
			case <-mReconnect.ClickedCh:
				mStatus.SetTitle("bladerunner: reconnecting…")
				go runnerRun("reconnect")
			case <-mRestart.ClickedCh:
				mStatus.SetTitle("bladerunner: restarting…")
				go restartVM()
			case <-mWeb.ClickedCh:
				// Under StartOnFirstAction a click while stopped lazily boots the
				// VM, then opens the web UI once the guest is healthy.
				if firstAction && vmHealth() == vmStopped {
					triggerStart()
					go runWhenHealthy(func() { _ = launchDetached("web") })
				} else {
					_ = launchDetached("web")
				}
			case <-mShell.ClickedCh:
				if firstAction && vmHealth() == vmStopped {
					triggerStart()
					go runWhenHealthy(openShellTerminal)
				} else {
					openShellTerminal()
				}
			case <-mSettings.ClickedCh:
				showSettingsWindow()
			case <-mQuit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
}

// engineUpgradeAvailable reports whether the running VM (started by some prior
// `br start`) is an OLDER build than this menubar (menubarVer) — i.e. a restart
// would move the engine up to the current version. Returns false when the VM
// isn't reachable or versions don't compare (dev/unknown), so it never nags
// spuriously.
func engineUpgradeAvailable(menubarVer string) bool {
	serverVer, err := control.NewClient(config.DefaultStateDir()).ServerVersion()
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
func vmHealth() vmState {
	c := control.NewClient(config.DefaultStateDir())
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
	case vmWedged, vmUnknown:
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
