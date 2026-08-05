package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/control"
	"github.com/stuffbucket/bladerunner/internal/vm"
)

var saveFlags struct {
	path string
}

var saveCmd = &cobra.Command{
	Use:   "save",
	Short: "Snapshot the running VM's state to a file",
	Long: `Pause the guest, write its machine state to disk, then resume it.

The resulting file can later be restored with 'br restore'. By default it is
written to <state-dir>/saved-state.bin; pass --path to choose another location.

Requires a host that supports VZ save/restore (macOS 14+); not all guest
configurations are eligible.`,
	RunE: runSave,
}

func init() {
	saveCmd.Flags().StringVar(&saveFlags.path, "path", "", "Destination file (default: <state-dir>/saved-state.bin)")
}

func runSave(_ *cobra.Command, _ []string) error {
	// Snapshot the instance the user selected, not whatever lives in the
	// default state dir. No prompt: offering to START a VM you asked to
	// snapshot would be nonsense.
	target, err := resolveInstanceTarget()
	if err != nil {
		return jsonOrError(err)
	}
	client := control.NewClient(target.StateDir)
	if !client.IsRunning() {
		return jsonOrError(notRunningError(target))
	}

	// keepPaused=false: a live snapshot — the server resumes the guest after
	// writing the state file.
	savedPath, err := client.SaveState(false)
	if err != nil {
		return jsonOrError(err)
	}

	finalPath := savedPath
	if saveFlags.path != "" && saveFlags.path != savedPath {
		// State file and metadata sidecar move as one generation, across
		// filesystems included. A destination that held only the state file
		// would be refused by 'br restore', so reporting it as saved would be a
		// lie the operator only discovers when they need the snapshot.
		if err := vm.MoveSavedState(savedPath, saveFlags.path); err != nil {
			return jsonOrError(fmt.Errorf("move saved state to %s: %w", saveFlags.path, err))
		}
		finalPath = saveFlags.path
	}

	if jsonOutput {
		return emitJSON(map[string]string{jsonFieldStatus: "saved", "path": finalPath})
	}
	fmt.Printf("%s VM state saved to %s\n", success("✓"), value(finalPath))
	return nil
}

// jsonOrError emits err as JSON when --json is set and returns it, so callers
// can `return jsonOrError(err)`.
func jsonOrError(err error) error {
	if jsonOutput {
		emitJSONError(err)
	}
	return err
}
