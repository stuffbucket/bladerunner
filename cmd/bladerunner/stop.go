package main

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/control"
)

var stopFlags struct {
	timeout int
	force   bool
}

// Force-stop timing. A panicked guest ignores ACPI shutdown, so the normal
// graceful path hangs; --force bounds that by escalating to SIGTERM then
// SIGKILL on the host process.
const (
	// forceGracePeriod is how long --force waits for graceful shutdown before
	// escalating.
	forceGracePeriod = 5 * time.Second
	// sigtermGrace / sigkillGrace bound how long we wait for the process to
	// exit after each signal.
	sigtermGrace = 3 * time.Second
	sigkillGrace = 2 * time.Second
)

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the running VM",
	Long: `Stop the running Bladerunner VM.

By default sends a graceful shutdown signal and waits. If the guest is
unresponsive (e.g. a kernel panic), graceful shutdown never completes; use
--force to escalate to terminating the host process after a short grace
period.`,
	RunE: runStop,
}

func init() {
	stopCmd.Flags().IntVarP(&stopFlags.timeout, "timeout", "t", config.DefaultStopTimeout, "Seconds to wait for graceful shutdown")
	stopCmd.Flags().BoolVarP(&stopFlags.force, "force", "f", false, "Force-stop: terminate the host process if graceful shutdown stalls (e.g. panicked guest)")
}

func runStop(_ *cobra.Command, _ []string) error {
	stateDir := config.DefaultStateDir()

	client := control.NewClient(stateDir)

	if !client.IsRunning() {
		err := fmt.Errorf("VM is not running")
		if jsonOutput {
			emitJSONError(err)
		}
		return err
	}

	// Capture the host PID up front, while the control server still answers —
	// --force needs it even if the server later wedges.
	hostPID := readHostPID(client)

	if !jsonOutput {
		fmt.Println("Stopping VM (sending graceful shutdown signal)...")
	}
	if err := client.StopVM(); err != nil {
		// Under --force a wedged server is exactly the case we handle below;
		// don't abort, fall through to the PID escalation.
		if !stopFlags.force {
			if jsonOutput {
				emitJSONError(err)
			}
			return err
		}
		if !jsonOutput {
			fmt.Printf("Graceful stop request failed (%v); will force-terminate.\n", err)
		}
	}

	socketPath := control.SocketPath(stateDir)

	// Graceful wait. With --force this is just the short grace period before
	// escalating; otherwise it is the full user-specified timeout.
	graceful := time.Duration(stopFlags.timeout) * time.Second
	if stopFlags.force && graceful > forceGracePeriod {
		graceful = forceGracePeriod
	}
	if !jsonOutput {
		fmt.Printf("Waiting up to %s for shutdown...\n", graceful.Round(time.Second))
	}
	if waitForSocketGone(socketPath, graceful) {
		if jsonOutput {
			return emitJSON(stopResult{Status: "stopped"})
		}
		fmt.Println("VM stopped")
		return nil
	}

	if !stopFlags.force {
		err := fmt.Errorf("timeout waiting for VM to stop (use 'br stop --force' to terminate a hung/panicked VM)")
		if jsonOutput {
			emitJSONError(err)
		}
		return err
	}

	return forceTerminate(socketPath, hostPID)
}

// stopResult is the JSON payload emitted by `br stop --json` on success.
type stopResult struct {
	Status string `json:"status"`           // "stopped" or "force-stopped"
	Signal string `json:"signal,omitempty"` // "SIGTERM"|"SIGKILL" on the force path
}

// readHostPID returns the host process PID reported by the control server, or
// 0 if unavailable.
func readHostPID(client *control.Client) int {
	v, err := client.GetConfig(control.ConfigKeyPID)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return pid
}

// waitForSocketGone polls until the control socket disappears (process exited)
// or the deadline passes. Returns true if the socket is gone.
func waitForSocketGone(socketPath string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); os.IsNotExist(err) {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	_, err := os.Stat(socketPath)
	return os.IsNotExist(err)
}

// forceTerminate escalates SIGTERM then SIGKILL on the host PID, then cleans up
// the stale control socket. Used only by --force.
func forceTerminate(socketPath string, pid int) error {
	if pid <= 0 {
		err := fmt.Errorf("cannot force-stop: host PID unknown (control server gave no pid)")
		if jsonOutput {
			emitJSONError(err)
		}
		return err
	}
	if !jsonOutput {
		fmt.Printf("Graceful shutdown stalled; force-terminating host process %d...\n", pid)
	}

	_ = syscall.Kill(pid, syscall.SIGTERM)
	if waitForProcessGone(pid, sigtermGrace) {
		cleanupSocket(socketPath)
		if jsonOutput {
			return emitJSON(stopResult{Status: "force-stopped", Signal: "SIGTERM"})
		}
		fmt.Println("VM force-stopped (SIGTERM)")
		return nil
	}

	_ = syscall.Kill(pid, syscall.SIGKILL)
	if waitForProcessGone(pid, sigkillGrace) {
		cleanupSocket(socketPath)
		if jsonOutput {
			return emitJSON(stopResult{Status: "force-stopped", Signal: "SIGKILL"})
		}
		fmt.Println("VM force-stopped (SIGKILL)")
		return nil
	}

	err := fmt.Errorf("failed to terminate host process %d", pid)
	if jsonOutput {
		emitJSONError(err)
	}
	return err
}

// waitForProcessGone polls signal 0 against pid until it no longer exists or
// the deadline passes. Returns true once the process is gone.
func waitForProcessGone(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return true // ESRCH: process no longer exists
		}
		time.Sleep(200 * time.Millisecond)
	}
	return syscall.Kill(pid, 0) != nil
}

// cleanupSocket removes a stale control socket left behind by a force-kill so
// later `br status`/`br start` don't see a dead listener.
func cleanupSocket(socketPath string) {
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) && !jsonOutput {
		fmt.Printf("note: could not remove stale control socket %s: %v\n", socketPath, err)
	}
}
