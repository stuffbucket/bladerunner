package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/control"
	"github.com/stuffbucket/bladerunner/internal/logging"
)

// errVMNotRunning is returned, with a clean message that omits the raw
// control-socket dial failure, when a command needs the VM but it is not running
// and was not started. The underlying socket error is logged at debug level.
var errVMNotRunning = errors.New("VM is not running; start it with 'br start'")

// vmStartReadyTimeout bounds how long requireRunningVM waits for an auto-started
// VM to publish its readiness signal.
const vmStartReadyTimeout = 3 * time.Minute

// requireRunningVM returns a control client for a running VM. If the VM is not
// running it offers to start it when attached to an interactive terminal;
// otherwise (or if the user declines) it returns errVMNotRunning. The raw
// control-socket dial failure is logged, never printed, so the terminal stays
// clean. Commands that need the VM (all except `status`) should funnel through
// this rather than touching the control client directly.
func requireRunningVM() (*control.Client, error) {
	stateDir := config.DefaultStateDir()
	client := control.NewClient(stateDir)
	if client.IsRunning() {
		return client, nil
	}
	// Log the detail for `BLADERUNNER_LOG_LEVEL=debug`; keep it off the terminal.
	logging.L().Debug("VM control socket unreachable; VM not running",
		"socket", control.SocketPath(stateDir))

	if !interactiveTerminal() {
		return nil, errVMNotRunning
	}
	if !confirmStartVM() {
		return nil, errVMNotRunning
	}
	if err := startVMDetachedAndWait(stateDir); err != nil {
		return nil, err
	}
	return client, nil
}

// interactiveTerminal reports whether both stdin and stdout are TTYs, so a
// [Y/n] prompt can be shown and answered.
func interactiveTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// confirmStartVM shows a [Y/n] prompt (default yes) asking to start the VM.
func confirmStartVM() bool {
	fmt.Printf("%s The VM is not running. Start it now? %s ", subtle("›"), subtle("[Y/n]"))
	return confirmStartVMFrom(os.Stdin)
}

// confirmStartVMFrom parses a [Y/n] answer (default yes on empty/EOF) from r.
func confirmStartVMFrom(r io.Reader) bool {
	line, err := bufio.NewReader(r).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		// Treat empty input as the default "yes" only when the line ended
		// cleanly (newline) or at EOF after no characters; a read error with a
		// partial line is declined below.
		if err != nil && line == "" {
			// EOF with no input → accept the default (yes).
			return errors.Is(err, io.EOF)
		}
		return true
	default:
		return false
	}
}

// startVMDetachedAndWait launches `br start` as a detached background process
// (so it outlives this short-lived command and becomes the VM host) and waits
// until the VM publishes its SSH config path — the signal that StartVM has
// returned and the VM is up — or the timeout elapses.
func startVMDetachedAndWait(stateDir string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate br executable: %w", err)
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", os.DevNull, err)
	}
	defer func() { _ = devnull.Close() }()

	// context.Background(): the child must outlive this short-lived command, so
	// it is intentionally not bound to a cancelable context.
	cmd := exec.CommandContext(context.Background(), exe, "start")
	cmd.Stdin = devnull
	cmd.Stdout = devnull
	cmd.Stderr = devnull
	detachProcess(cmd) // platform-specific: run in a new session
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start VM: %w", err)
	}
	pid := cmd.Process.Pid
	// The started process is the long-lived VM host; do not wait on it.
	_ = cmd.Process.Release()

	fmt.Printf("%s Starting VM (pid %d)…\n", subtle("›"), pid)

	client := control.NewClient(stateDir)
	deadline := time.Now().Add(vmStartReadyTimeout)
	for time.Now().Before(deadline) {
		if client.IsRunning() {
			if v, _ := client.GetConfig(control.ConfigKeySSHConfigPath); v != "" {
				fmt.Println(success("✓ VM is running"))
				return nil
			}
		}
		time.Sleep(750 * time.Millisecond)
	}
	return errors.New("timed out waiting for the VM to start; check 'br status' and the log")
}
