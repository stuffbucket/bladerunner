//go:build darwin

package main

import (
	"bytes"
	"context"
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

// wakeGapSeconds: if wall-clock advances far more than the poll interval between
// two polls, the Mac slept and woke (the agent was frozen meanwhile). On wake we
// auto-reconnect to heal the guest's clock + vsock forwarders.
const wakeGapSeconds = 60

// status-dot rendering constants.
const (
	iconSize    = 22
	iconInset   = 3.0
	pixelCenter = 0.5
	alphaOpaque = 0xFF
	// dot colors: gray (stopped), green (running+healthy), amber (running but
	// the guest is not answering — "wedged" — or status unknown).
	grayR, grayG, grayB    = 0x9A, 0xA4, 0xA8
	greenR, greenG, greenB = 0x27, 0xC9, 0x3F
	amberR, amberG, amberB = 0xFF, 0xBD, 0x2E
)

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
	// systray.Run takes over the main thread and blocks until Quit.
	systray.Run(onMenubarReady, func() {})
	return nil
}

func onMenubarReady() {
	systray.SetIcon(statusIcon(vmStopped))
	systray.SetTooltip("bladerunner")

	mStatus := systray.AddMenuItem("Checking…", "Bladerunner VM status")
	mStatus.Disable()
	systray.AddSeparator()
	mStart := systray.AddMenuItem("Start VM", "Boot the bladerunner VM")
	mStop := systray.AddMenuItem("Stop VM", "Gracefully stop the VM")
	mReconnect := systray.AddMenuItem("Reconnect", "Re-sync the guest after sleep (clock + forwarders) without restarting")
	mRestart := systray.AddMenuItem("Restart VM", "Stop and start the VM (fixes a wedged/unresponsive guest)")
	systray.AddSeparator()
	mWeb := systray.AddMenuItem("Open Web UI…", "Open the Incus web UI with single sign-on")
	mShell := systray.AddMenuItem("Open Shell…", "Open a Terminal shell inside the VM")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Quit the bladerunner menubar")

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
		healthy := st == vmHealthy
		setEnabled(mStart, st == vmStopped)
		setEnabled(mStop, running)
		setEnabled(mReconnect, running) // light heal after sleep
		setEnabled(mRestart, running)   // restart is the fix when fully wedged
		setEnabled(mWeb, healthy)       // web/shell only work when the guest answers
		setEnabled(mShell, healthy)
	}
	apply(vmStopped)

	// Poll health off the click loop so a slow probe (a wedged guest) never
	// blocks the menu. The same loop detects host sleep/wake (a big wall-clock
	// jump between polls) and auto-reconnects to heal the guest.
	healthCh := make(chan vmState, 1)
	go func() {
		lastWall := time.Now().Unix()
		for {
			st := vmHealth()
			now := time.Now().Unix()
			if st != vmStopped && now-lastWall > int64(menubarRefreshInterval/time.Second)+wakeGapSeconds {
				go runnerRun("reconnect") // self-heal after a detected wake
			}
			lastWall = now
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
			case <-mStart.ClickedCh:
				mStatus.SetTitle("bladerunner: starting…")
				_ = launchDetached("start")
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
				_ = launchDetached("web")
			case <-mShell.ClickedCh:
				openShellTerminal()
			case <-mQuit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
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
	return "runner"
}

// launchDetached starts `runner <args...>` detached (new session) so a
// long-running command (start, web) keeps running after the menu action returns.
func launchDetached(args ...string) error {
	cmd := exec.CommandContext(context.Background(), runnerSelf(), args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// Detached: the process should outlive this short-lived context.
	cmd.Cancel = func() error { return nil }
	return cmd.Start()
}

// runnerRun runs `runner <args...>` to completion (for short commands like stop).
func runnerRun(args ...string) {
	_ = exec.CommandContext(context.Background(), runnerSelf(), args...).Run()
}

// openShellTerminal opens Terminal.app running `runner shell`.
func openShellTerminal() {
	script := fmt.Sprintf("tell application \"Terminal\"\n  do script \"%s shell\"\n  activate\nend tell", runnerSelf())
	_ = exec.CommandContext(context.Background(), "osascript", "-e", script).Start()
}

// statusIcon renders a small filled-circle status dot colored by VM state.
// Generated in-code so there are no asset files.
func statusIcon(state vmState) []byte {
	img := image.NewRGBA(image.Rect(0, 0, iconSize, iconSize))
	dot := color.RGBA{R: grayR, G: grayG, B: grayB, A: alphaOpaque}
	switch state {
	case vmHealthy:
		dot = color.RGBA{R: greenR, G: greenG, B: greenB, A: alphaOpaque}
	case vmWedged, vmUnknown:
		dot = color.RGBA{R: amberR, G: amberG, B: amberB, A: alphaOpaque}
	case vmStopped:
		// gray (default)
	}
	cx, cy := float64(iconSize)/2, float64(iconSize)/2
	r := float64(iconSize)/2 - iconInset
	for y := range iconSize {
		for x := range iconSize {
			dx, dy := float64(x)+pixelCenter-cx, float64(y)+pixelCenter-cy
			if dx*dx+dy*dy <= r*r {
				img.Set(x, y, dot)
			}
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
