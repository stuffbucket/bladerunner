package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/control"
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
	client := control.NewClient(config.DefaultStateDir())
	if !client.IsRunning() {
		return jsonOrError(fmt.Errorf("VM is not running"))
	}

	// keepPaused=false: a live snapshot — the server resumes the guest after
	// writing the state file.
	savedPath, err := client.SaveState(false)
	if err != nil {
		return jsonOrError(err)
	}

	finalPath := savedPath
	if saveFlags.path != "" && saveFlags.path != savedPath {
		if err := os.Rename(savedPath, saveFlags.path); err != nil {
			return jsonOrError(fmt.Errorf("move saved state to %s: %w", saveFlags.path, err))
		}
		finalPath = saveFlags.path
	}

	if jsonOutput {
		return emitJSON(map[string]string{"status": "saved", "path": finalPath})
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
