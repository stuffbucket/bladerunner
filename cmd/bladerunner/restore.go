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
	Long: `Bring the VM up from a saved-state file (see 'br save') and resume the
guest where it left off, instead of cold-booting.

The VM configuration (CPUs, memory, disk) must match the one that produced the
saved state; pass the same start flags if you used non-default values.

This is a foreground process, like 'br start'. The VM must not already be
running.`,
	RunE: runRestore,
}

func init() {
	restoreCmd.Flags().StringVar(&restoreFlags.path, "path", "", "Saved-state file (default: <state-dir>/saved-state.bin)")
	// Accept the same resource flags as start so the restored config can match.
	restoreCmd.Flags().UintVar(&startFlags.cpus, "cpus", config.DefaultCPUs, "Number of CPUs (must match the saved VM)")
	restoreCmd.Flags().Uint64Var(&startFlags.memory, "memory", config.DefaultMemoryGiB, "Memory in GiB (must match the saved VM)")
	restoreCmd.Flags().IntVar(&startFlags.disk, "disk", config.DefaultDiskSizeGiB, "Disk size in GiB (must match the saved VM)")
}

func runRestore(cmd *cobra.Command, args []string) error {
	if control.NewClient(config.DefaultStateDir()).IsRunning() {
		return jsonOrError(fmt.Errorf("VM is already running; stop it first ('br stop') before restoring"))
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
		return jsonOrError(fmt.Errorf("saved state not found at %s (run 'br save' first): %w", path, err))
	}

	// Hand off to the start flow in restore mode.
	startFlags.restoreFrom = path
	return runStart(cmd, args)
}
