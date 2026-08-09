package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/control"
)

var restartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Stop the running VM and start it again",
	Long: `Stop the running Bladerunner VM and start it again.

This is 'br stop' followed by 'br start', which is what it was before this verb
existed. The guest is asked to power itself off and the host waits for it to
reach the stopped state, so the disk image is left consistent; --timeout and
--force behave exactly as they do on 'br stop'.

The VM comes back with the settings on disk, NOT with any one-off flags the
original 'br start' was given. Those flags are not persisted, so a VM started
with '--cpus 8' restarts at whatever 'br config' says. Change the setting first
if the shape should survive.

Restarting a VM that is not running just starts one.`,
	Args: cobra.NoArgs,
	RunE: runRestart,
}

func init() {
	// Mirrors br stop. The stop half is the half that can hang, and an operator
	// reaching for restart after a wedged guest needs the same escape hatch.
	restartCmd.Flags().IntVarP(&stopFlags.timeout, "timeout", "t", control.DefaultEjectTimeoutSeconds,
		"Seconds to let the guest power itself off before forcing the stop")
	restartCmd.Flags().BoolVar(&stopFlags.force, "force", false,
		"Skip the graceful shutdown and force the stop immediately")
}

// runRestart stops a running VM and starts it again.
//
// It reuses runStop and runStart rather than reimplementing either. A restart
// that drifted from the behavior of its two halves would be the worst kind of
// convenience verb: the one that does ALMOST what you would have typed.
func runRestart(cmd *cobra.Command, args []string) error {
	stateDir, err := targetStateDir()
	if err != nil {
		return err
	}

	// Not running is not an error. Someone typing restart wants a running VM at
	// the end of it, and refusing here would send them to a second command to
	// reach the state they already asked for.
	//
	// The gate is the liveness ladder, not a ping: a wedged holder answers
	// nothing, so a ping-shaped gate would skip the stop and go straight to a
	// start that the wedged holder's own start lock then refuses.
	if instanceHeld(stateDir) {
		if err := runStop(cmd, args); err != nil {
			return fmt.Errorf("restart: stop: %w", err)
		}
	} else if !jsonOutput {
		fmt.Println(subtle("No VM was running; starting one."))
	}

	if err := runStart(cmd, args); err != nil {
		return fmt.Errorf("restart: start: %w", err)
	}
	return nil
}
