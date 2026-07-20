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
	Short: "Nudge the guest clock back into sync after a host sleep, without restarting",
	Long: `Heal the running VM's clock after the Mac has slept.

macOS sleep freezes the guest without a clean suspend, leaving the guest clock
behind real time — which breaks the web UI's OIDC token exchange. The guest
watchdog owns post-sleep recovery autonomously (it kicks chrony on the next
60s cycle and bounces any wedged vsock relay). 'reconnect' is a manual, faster
version of that clock kick: it forces a fresh chrony measurement and steps the
clock NOW rather than waiting for the watchdog cycle. The authoritative time
source is the host, reached over the SNTP-over-vsock bridge; chrony
re-disciplines against it. It is best-effort and never restarts the VM: if the
guest is fully unresponsive, use 'br stop' + 'br start' (or the menubar's
Restart).`,
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

	// One duty: kick chrony to re-measure against the host (over the vsock SNTP
	// bridge) and step the clock immediately. 'burst 4/4' forces a fresh sample
	// without waiting for chrony's autonomous poll; 'makestep' then applies any
	// offset as a step (a step never disturbs CLOCK_MONOTONIC, so timers and
	// containers are unaffected). We no longer stamp the wall clock with
	// 'date -s @epoch' (TZ/DST-fragile, and needed a sudo-date path) nor bounce
	// the relays here: the NTP relay is Restart=always and the watchdog owns
	// wedged-relay recovery, so a manual clock kick is all reconnect needs to be.
	if err := guestExec(configPath, sudoCmd, "-n", "sh", "-c", "chronyc burst 4/4 && chronyc makestep"); err != nil {
		err = fmt.Errorf("reconnect failed (guest may be fully unresponsive — try a restart): %w", err)
		if jsonOutput {
			emitJSONError(err)
		}
		return err
	}

	if jsonOutput {
		return emitJSON(map[string]string{jsonFieldStatus: "reconnected"})
	}
	fmt.Printf("%s Nudged the guest clock back into sync (chrony re-measured + stepped)\n", success("✓"))
	return nil
}

// guestExec runs a single non-interactive command in the guest over the vsock
// SSH path, failing fast if the guest is unreachable.
func guestExec(configPath string, args ...string) error {
	sshPath, argv, err := sshArgv(configPath, []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10"}, args...)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), guestExecTimeout)
	defer cancel()
	// Drop argv[0] ("ssh"); CommandContext prepends the resolved path itself.
	out, err := exec.CommandContext(ctx, sshPath, argv[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("guest %v: %w: %s", args, err, out)
	}
	return nil
}
