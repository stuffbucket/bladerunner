package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/control"
)

var restoreFlags struct {
	path string
}

var restoreCmd = &cobra.Command{
	Use:   "restore",
	Short: "Start the VM by restoring a previously saved state",
	Long: `Bring the VM up from a saved-state file (see 'runner save') and resume the
guest where it left off, instead of cold-booting.

The CPU/memory/disk configuration is read from the saved state's metadata, so
you don't need to re-specify it. The restore is refused if the disk image has
changed since the snapshot (which would corrupt the guest).

This is a foreground process, like 'runner start'. The VM must not already be
running.`,
	RunE: runRestore,
}

func init() {
	restoreCmd.Flags().StringVar(&restoreFlags.path, "path", "", "Saved-state file (default: <state-dir>/saved-state.bin)")
}

func runRestore(cmd *cobra.Command, args []string) error {
	if control.NewClient(config.DefaultStateDir()).IsRunning() {
		return jsonOrError(fmt.Errorf("VM is already running; stop it first ('runner stop') before restoring"))
	}

	path := restoreFlags.path
	if path == "" {
		cfg, err := config.Default(startFlags.stateDir)
		if err != nil {
			return jsonOrError(err)
		}
		path = cfg.SavedStatePath
	}
	if _, err := os.Stat(path); err != nil {
		return jsonOrError(fmt.Errorf("saved state not found at %s (run 'runner save' first): %w", path, err))
	}

	// Hand off to the start flow in restore mode.
	startFlags.restoreFrom = path
	return runStart(cmd, args)
}
