package main

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/control"
)

var stopFlags struct {
	timeout int
	force   bool
}

// Force-stop timing. A panicked guest ignores ACPI shutdown, so the normal
// graceful path hangs; --force bounds that by asking for an immediate forced
// stop and, if even that stalls, escalating to SIGTERM then SIGKILL on the host
// process.
const (
	// forceGracePeriod is how long --force waits for the forced shutdown to
	// complete before escalating to signals.
	forceGracePeriod = 5 * time.Second
	// sigtermGrace / sigkillGrace bound how long we wait for the process to
	// exit after each signal.
	sigtermGrace = 3 * time.Second
	sigkillGrace = 2 * time.Second
	// stopTeardownMargin is added to the guest drain budget when waiting for the
	// control socket to disappear: after the guest powers off the host still has
	// to flush the disk image, close the forwarders and exit.
	stopTeardownMargin = 15 * time.Second
	// maxDrainRequest caps the drain budget we ask the server for, staying under
	// the control client's per-command read timeout (10 minutes) so a very large
	// --timeout cannot turn into a client-side transport error.
	maxDrainRequest = 9 * time.Minute
	// stopPollInterval is how often the shutdown wait re-checks the control
	// socket.
	stopPollInterval = 500 * time.Millisecond
)

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the running VM",
	Long: `Stop the running Bladerunner VM.

By default the guest is asked to power itself off (ACPI) and the host waits for
it to actually reach the stopped state, so the disk image is left consistent.
--timeout bounds that guest drain: when it expires the VM is force-stopped (a
power cut) and that is reported. If the guest is unresponsive (e.g. a kernel
panic), use --force to skip straight to the forced stop, escalating to
terminating the host process if even that stalls.`,
	RunE: runStop,
}

func init() {
	// The drain budget has to cover a real guest shutdown (stopping Incus,
	// unmounting and flushing filesystems), so default to the same 60s the
	// control plane uses for eject rather than a shorter ceiling.
	stopCmd.Flags().IntVarP(&stopFlags.timeout, "timeout", "t", control.DefaultEjectTimeoutSeconds, "Seconds to let the guest power itself off before forcing the stop")
	stopCmd.Flags().BoolVarP(&stopFlags.force, "force", "f", false, "Force-stop: cut power to the guest immediately, terminating the host process if that stalls (e.g. panicked guest)")
}

func runStop(_ *cobra.Command, _ []string) error {
	stateDir, err := targetStateDir()
	if err != nil {
		return err
	}

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

	drain := drainBudget(stopFlags.timeout)
	if !jsonOutput {
		if stopFlags.force {
			fmt.Println("Stopping VM (forced: cutting power to the guest)...")
		} else {
			fmt.Printf("Stopping VM (asking the guest to power off, up to %s)...\n", drain.Round(time.Second))
		}
	}
	// The server answers a shutdown request only once the guest has drained, so
	// issue it in the background: the control socket disappearing is the real
	// completion signal, and this keeps a wedged server from outlasting our own
	// budget (its per-command read timeout is measured in minutes).
	reqErr := make(chan error, 1)
	go func() { reqErr <- requestStop(client, drain, stopFlags.force) }()

	socketPath := control.SocketPath(stateDir)

	// The server drains the guest and only then tears the VMM down, so allow the
	// drain budget plus the teardown margin. With --force this is just the short
	// grace period before escalating to signals.
	wait := drain + stopTeardownMargin
	if stopFlags.force {
		wait = forceGracePeriod
	}
	if !jsonOutput {
		fmt.Printf("Waiting up to %s for shutdown...\n", wait.Round(time.Second))
	}
	stopped, err := waitForStop(socketPath, wait, reqErr)
	if stopped {
		if jsonOutput {
			return emitJSON(stopResult{Status: control.StatusStopped})
		}
		fmt.Println("VM stopped")
		return nil
	}

	if err == nil {
		err = fmt.Errorf("timeout waiting for VM to stop (use 'br stop --force' to terminate a hung/panicked VM)")
	}
	if !stopFlags.force {
		if jsonOutput {
			emitJSONError(err)
		}
		return err
	}
	if !jsonOutput {
		fmt.Printf("Stop did not complete (%v); force-terminating.\n", err)
	}

	return forceTerminate(socketPath, hostPID)
}

// drainBudget converts a --timeout value in seconds into the budget the guest
// gets to power itself off, clamped to something the control client can
// actually wait for.
func drainBudget(timeoutSeconds int) time.Duration {
	budget := time.Duration(timeoutSeconds) * time.Second
	if budget <= 0 {
		budget = control.DefaultEjectTimeoutSeconds * time.Second
	}
	if budget > maxDrainRequest {
		budget = maxDrainRequest
	}
	return budget
}

// waitForStop waits for the shutdown to complete: the control socket
// disappearing means the server drained the guest and exited (stopped=true). If
// the stop request itself comes back with an error first, nothing more is going
// to happen, so report it instead of burning the whole budget.
func waitForStop(socketPath string, within time.Duration, reqErr <-chan error) (bool, error) {
	deadline := time.Now().Add(within)
	for {
		if socketGone(socketPath) {
			return true, nil
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		select {
		case err := <-reqErr:
			reqErr = nil // drained: keep waiting on the socket, not this channel
			if err != nil {
				return socketGone(socketPath), err
			}
		case <-time.After(stopPollInterval):
		}
	}
}

// socketGone reports whether the control socket has been removed.
func socketGone(socketPath string) bool {
	_, err := os.Stat(socketPath)
	return os.IsNotExist(err)
}

// requestStop asks the server to shut the guest down, carrying our drain budget
// so --timeout bounds the guest's power-off rather than only our own wait. The
// budget rides on the eject-shaped control command, the only shutdown command in
// the protocol that takes a timeout; a server that does not understand it falls
// back to the plain stop command, which drains with the server-side default.
func requestStop(client *control.Client, drain time.Duration, force bool) error {
	err := client.Eject(force, int(drain/time.Second))
	if err == nil {
		return nil
	}
	if fallbackErr := client.StopVM(); fallbackErr != nil {
		return fmt.Errorf("stop request failed: %w (fallback stop: %w)", err, fallbackErr)
	}
	return nil
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
		if socketGone(socketPath) {
			return true
		}
		time.Sleep(stopPollInterval)
	}
	return socketGone(socketPath)
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
