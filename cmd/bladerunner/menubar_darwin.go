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

// status-dot rendering constants.
const (
	iconSize    = 22
	iconInset   = 3.0
	pixelCenter = 0.5
	alphaOpaque = 0xFF
	// gray (stopped) and green (running) dot colors.
	grayR, grayG, grayB    = 0x9A, 0xA4, 0xA8
	greenR, greenG, greenB = 0x27, 0xC9, 0x3F
)

func runMenubar() error {
	// systray.Run takes over the main thread and blocks until Quit.
	systray.Run(onMenubarReady, func() {})
	return nil
}

func onMenubarReady() {
	systray.SetIcon(statusIcon(false))
	systray.SetTooltip("bladerunner")

	mStatus := systray.AddMenuItem("Checking…", "Bladerunner VM status")
	mStatus.Disable()
	systray.AddSeparator()
	mStart := systray.AddMenuItem("Start VM", "Boot the bladerunner VM")
	mStop := systray.AddMenuItem("Stop VM", "Gracefully stop the VM")
	systray.AddSeparator()
	mWeb := systray.AddMenuItem("Open Web UI…", "Open the Incus web UI with single sign-on")
	mShell := systray.AddMenuItem("Open Shell…", "Open a Terminal shell inside the VM")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Quit the bladerunner menubar")

	refresh := func() {
		running := vmRunning()
		systray.SetIcon(statusIcon(running))
		if running {
			mStatus.SetTitle("bladerunner: running")
		} else {
			mStatus.SetTitle("bladerunner: stopped")
		}
		setEnabled(mStart, !running)
		setEnabled(mStop, running)
		setEnabled(mWeb, running)
		setEnabled(mShell, running)
	}
	refresh()

	go func() {
		ticker := time.NewTicker(menubarRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				refresh()
			case <-mStart.ClickedCh:
				mStatus.SetTitle("bladerunner: starting…")
				_ = launchDetached("start")
			case <-mStop.ClickedCh:
				mStatus.SetTitle("bladerunner: stopping…")
				go runnerRun("stop")
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

// vmRunning reports whether the default-state-dir VM's control socket is live.
func vmRunning() bool {
	return control.NewClient(config.DefaultStateDir()).IsRunning()
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

// statusIcon renders a small filled-circle status dot: green when the VM is
// running, gray when stopped. Generated in-code so there are no asset files.
func statusIcon(running bool) []byte {
	img := image.NewRGBA(image.Rect(0, 0, iconSize, iconSize))
	dot := color.RGBA{R: grayR, G: grayG, B: grayB, A: alphaOpaque}
	if running {
		dot = color.RGBA{R: greenR, G: greenG, B: greenB, A: alphaOpaque}
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
