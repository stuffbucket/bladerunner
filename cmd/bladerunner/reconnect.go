package main

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/spf13/cobra"
)

const guestExecTimeout = 20 * time.Second

// sudoCmd is the privilege-escalation command used to run guest/host commands
// that require root.
const sudoCmd = "sudo"

var reconnectCmd = &cobra.Command{
	Use:   "reconnect",
	Short: "Re-sync the guest after a host sleep (clock + Incus/OIDC forwarders), without restarting",
	Long: `Heal the running VM after the Mac has slept.

macOS sleep freezes the guest without a clean suspend, leaving the guest clock
behind real time — which breaks the web UI's OIDC token exchange — and the
Incus/OIDC vsock forwarders stale. 'reconnect' pushes the host's current time
into the guest, kicks NTP, and restarts the Incus/OIDC vsock forwarders, all
without restarting the VM. It is best-effort: if the guest is already fully
unresponsive, use 'br stop' + 'br start' (or the menubar's Restart).`,
	RunE: runReconnect,
}

func runReconnect(_ *cobra.Command, _ []string) error {
	configPath, err := sshConfigFromControl()
	if err != nil {
		if jsonOutput {
			emitJSONError(err)
		}
		return err
	}

	// Steps, in order: push host time so OIDC tokens validate immediately (an
	// absolute UTC epoch — TZ/DST-independent), then restart chrony (now the time
	// source; timesyncd is masked) and bounce the stale vsock forwarders — incl.
	// the host-time NTP bridge — so chrony re-disciplines against the host. We
	// deliberately do NOT restart bladerunner-vsock-ssh (we are connected through
	// it) or systemd-networkd (avoid disrupting containers).
	epoch := time.Now().Unix()
	steps := [][]string{
		{sudoCmd, "-n", "date", "-s", fmt.Sprintf("@%d", epoch)},
		{sudoCmd, "-n", "systemctl", "restart", "chrony", "bladerunner-vsock-ntp", "bladerunner-vsock-incus", "bladerunner-vsock-oidc"},
	}

	for _, step := range steps {
		if err := guestExec(configPath, step...); err != nil {
			err = fmt.Errorf("reconnect failed (guest may be fully unresponsive — try a restart): %w", err)
			if jsonOutput {
				emitJSONError(err)
			}
			return err
		}
	}

	if jsonOutput {
		return emitJSON(map[string]string{jsonFieldStatus: "reconnected"})
	}
	fmt.Printf("%s Re-synced the guest (pushed host time, restarted Incus/OIDC forwarders)\n", success("✓"))
	return nil
}

// guestExec runs a single non-interactive command in the guest over the vsock
// SSH path, failing fast if the guest is unreachable.
func guestExec(configPath string, args ...string) error {
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh not found: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), guestExecTimeout)
	defer cancel()
	full := append([]string{"-F", configPath, "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", sshHostAlias}, args...)
	out, err := exec.CommandContext(ctx, sshPath, full...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("guest %v: %w: %s", args, err, out)
	}
	return nil
}
