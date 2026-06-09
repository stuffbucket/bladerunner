package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/config"
)

var resetCmd = &cobra.Command{
	Use:   "reset",
	Short: "Reset VM to baseline state",
	Long: `Reset the Bladerunner VM by removing its disk and cloud-init files, allowing a fresh start.
This keeps the base image, SSH keys, and client certificates intact.

Use --full to also remove the base image (will be re-downloaded on next start).
Use --all to remove everything including keys and certificates.`,
	RunE: runReset,
}

var resetFlags struct {
	full    bool
	all     bool
	confirm bool
}

func init() {
	resetCmd.Flags().BoolVar(&resetFlags.full, "full", false, "Also remove the base image")
	resetCmd.Flags().BoolVar(&resetFlags.all, "all", false, "Remove everything (complete reset)")
	resetCmd.Flags().BoolVarP(&resetFlags.confirm, "yes", "y", false, "Skip confirmation prompt")
}

func runReset(_ *cobra.Command, _ []string) error {
	stateDir := config.DefaultStateDir()

	if _, err := os.Stat(stateDir); os.IsNotExist(err) {
		if jsonOutput {
			return emitJSON(resetResult{Status: "no-vm", Directory: stateDir})
		}
		fmt.Printf("No VM found at %s\n", stateDir)
		return nil
	}

	toRemove, resetType := resetFileList(resetFlags.full, resetFlags.all)

	if !jsonOutput {
		fmt.Printf("Resetting VM (%s reset)\n", resetType)
		fmt.Printf("Directory: %s\n\n", stateDir)
	}

	existingFiles := existingResetFiles(stateDir, toRemove)
	if len(existingFiles) == 0 {
		if jsonOutput {
			return emitJSON(resetResult{Status: "nothing-to-reset", Type: resetType, Directory: stateDir})
		}
		fmt.Println("Nothing to reset - VM is already at baseline state.")
		return nil
	}

	if !jsonOutput {
		fmt.Println("Files to remove:")
		for _, f := range existingFiles {
			fmt.Printf("  - %s\n", f)
		}
	}

	// In JSON mode there is no interactive prompt; require --yes so we never
	// block waiting on stdin while emitting machine-readable output.
	if !resetFlags.confirm {
		if jsonOutput {
			err := fmt.Errorf("reset requires --yes when --json is set (cannot prompt for confirmation)")
			emitJSONError(err)
			return err
		}
		if !confirmReset() {
			fmt.Println("Aborted.")
			return nil
		}
	}

	outcome := applyReset(stateDir, existingFiles, resetFlags.all)

	if jsonOutput {
		return emitJSON(resetResult{
			Status:        "reset",
			Type:          resetType,
			Directory:     stateDir,
			Removed:       len(outcome.removed),
			Failed:        len(outcome.failed),
			RemovedFiles:  outcome.removed,
			DirectoryGone: outcome.dirGone,
		})
	}

	reportResetHuman(outcome, resetFlags.all)
	return nil
}

// resetFileList returns the files to remove for the requested reset level and a
// human label for that level.
func resetFileList(full, all bool) ([]string, string) {
	files := []string{
		"disk.raw",
		"efi-vars.bin",
		"machine-id.bin",
		"cloud-init.iso",
		"cloud-init/user-data",
		"cloud-init/meta-data",
		"console.log",
		"startup-report.json",
		"runtime-metadata.json",
		"bladerunner.log",
	}
	if full || all {
		files = append(files, "base-image.raw")
	}
	if all {
		files = append(files, "client.crt", "client.key", "incus-client-example.go")
	}

	typ := "baseline"
	switch {
	case all:
		typ = "complete"
	case full:
		typ = "full"
	}
	return files, typ
}

// existingResetFiles filters candidates down to those that exist under stateDir.
func existingResetFiles(stateDir string, candidates []string) []string {
	var existing []string
	for _, f := range candidates {
		if _, err := os.Stat(filepath.Join(stateDir, f)); err == nil {
			existing = append(existing, f)
		}
	}
	return existing
}

// resetOutcome records the result of removing reset files.
type resetOutcome struct {
	removed []string
	failed  []string
	dirGone bool
}

// applyReset removes the given files under stateDir, prunes the empty cloud-init
// directory, and (for a complete reset) removes the now-empty state directory.
func applyReset(stateDir string, existing []string, all bool) resetOutcome {
	out := resetOutcome{removed: make([]string, 0, len(existing))}
	for _, f := range existing {
		if err := os.Remove(filepath.Join(stateDir, f)); err != nil {
			out.failed = append(out.failed, f)
		} else {
			out.removed = append(out.removed, f)
		}
	}

	cloudInitDir := filepath.Join(stateDir, "cloud-init")
	if entries, err := os.ReadDir(cloudInitDir); err == nil && len(entries) == 0 {
		_ = os.Remove(cloudInitDir)
	}

	if all {
		if entries, err := os.ReadDir(stateDir); err == nil && len(entries) == 0 {
			_ = os.Remove(stateDir)
			out.dirGone = true
		}
	}
	return out
}

// reportResetHuman prints the human-readable reset summary.
func reportResetHuman(o resetOutcome, all bool) {
	for _, f := range o.failed {
		fmt.Printf("  ✗ Failed to remove %s\n", f)
	}
	if o.dirGone {
		fmt.Printf("\n✓ Removed empty VM directory\n")
	}
	fmt.Printf("\n✓ Reset complete: %d files removed", len(o.removed))
	if len(o.failed) > 0 {
		fmt.Printf(", %d failed", len(o.failed))
	}
	fmt.Println()
	if !all {
		fmt.Printf("\nRun 'br start' to create a fresh VM.\n")
	}
}

// resetResult is the JSON payload emitted by `br reset --json`. Status is one
// of "no-vm", "nothing-to-reset", or "reset".
type resetResult struct {
	Status        string   `json:"status"`
	Type          string   `json:"type,omitempty"` // "baseline"|"full"|"complete"
	Directory     string   `json:"directory,omitempty"`
	Removed       int      `json:"removed"`
	Failed        int      `json:"failed"`
	RemovedFiles  []string `json:"removed_files,omitempty"`
	DirectoryGone bool     `json:"directory_gone,omitempty"`
}

// confirmReset prompts the user for confirmation.
// Returns true if user confirms, false otherwise.
func confirmReset() bool {
	fmt.Print("\nProceed? [y/N] ")
	var response string
	if _, err := fmt.Scanln(&response); err != nil {
		return false
	}
	return response == "y" || response == "Y"
}
