package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/control"
)

const upgradeStopTimeout = 30 * time.Second

var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Upgrade the running server to this binary's version",
	Long: `Replace the running control server with this binary when their versions
differ (i.e. you rebuilt or 'brew upgrade'd br while a VM was running).

When the host supports VZ save/restore, the guest's running state is saved,
the old server is stopped, and the new server restores and resumes the guest —
no cold reboot. Otherwise it falls back to a stop + fresh start.

This becomes the new long-lived server (foreground), like 'br start'.`,
	RunE: runUpgrade,
}

func runUpgrade(cmd *cobra.Command, args []string) error {
	stateDir := config.DefaultStateDir()
	client := control.NewClient(stateDir)
	if !client.IsRunning() {
		return jsonOrError(fmt.Errorf("no running server to upgrade (use 'br start')"))
	}

	serverVer, err := client.ServerVersion()
	if err != nil {
		return jsonOrError(fmt.Errorf("query server version: %w", err))
	}
	if serverVer == version {
		if jsonOutput {
			return emitJSON(map[string]string{jsonFieldStatus: "up-to-date", "version": version})
		}
		fmt.Printf("%s Server already up to date (%s)\n", success("✓"), value(version))
		return nil
	}

	if !jsonOutput {
		fmt.Printf("Upgrading server %s → %s\n", subtle(serverVer), value(version))
	}

	// Try a live save (keeping the guest paused so the disk stays consistent
	// with the saved RAM). The saved-state sidecar records the hardware config,
	// so the restored server rebuilds a matching configuration automatically.
	// If save/restore isn't supported, fall back to a cold restart.
	savedPath, saveErr := client.SaveState(true)
	if saveErr != nil {
		return upgradeColdRestart(cmd, args, client, stateDir, saveErr)
	}

	if err := stopAndWait(client, stateDir); err != nil {
		return jsonOrError(err)
	}

	if !jsonOutput {
		fmt.Println(subtle("Restoring guest into the upgraded server..."))
	}
	startFlags.restoreFrom = savedPath
	return runStart(cmd, args)
}

// upgradeColdRestart stops the old server and starts a fresh one (no restore),
// used when live save/restore is unavailable.
func upgradeColdRestart(cmd *cobra.Command, args []string, client *control.Client, stateDir string, saveErr error) error {
	if !jsonOutput {
		fmt.Printf("%s Live save/restore unavailable (%v); falling back to a cold restart.\n", warning("⚠"), saveErr)
	}
	if err := stopAndWait(client, stateDir); err != nil {
		return jsonOrError(err)
	}
	startFlags.restoreFrom = ""
	return runStart(cmd, args)
}

// stopAndWait stops the running server and waits for its control socket to
// disappear, confirming the process exited and released the VM.
func stopAndWait(client *control.Client, stateDir string) error {
	if err := client.StopVM(); err != nil {
		return fmt.Errorf("stop old server: %w", err)
	}
	if !waitForSocketGone(control.SocketPath(stateDir), upgradeStopTimeout) {
		return fmt.Errorf("old server did not exit within %s", upgradeStopTimeout)
	}
	return nil
}
