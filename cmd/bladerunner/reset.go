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

func runReset(cmd *cobra.Command, args []string) error {
	stateDir := config.DefaultStateDir()

	if _, err := os.Stat(stateDir); os.IsNotExist(err) {
		fmt.Printf("No VM found at %s\n", stateDir)
		return nil
	}

	// Define what to remove at each level
	baselineFiles := []string{
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

	fullFiles := []string{
		"base-image.raw",
	}

	allFiles := []string{
		"client.crt",
		"client.key",
		"incus-client-example.go",
	}

	// Build list of files to remove
	var toRemove []string
	toRemove = append(toRemove, baselineFiles...)
	if resetFlags.full || resetFlags.all {
		toRemove = append(toRemove, fullFiles...)
	}
	if resetFlags.all {
		toRemove = append(toRemove, allFiles...)
	}

	// Show what will be removed
	resetType := "baseline"
	if resetFlags.all {
		resetType = "complete"
	} else if resetFlags.full {
		resetType = "full"
	}

	fmt.Printf("Resetting VM (%s reset)\n", resetType)
	fmt.Printf("Directory: %s\n\n", stateDir)

	var existingFiles []string
	for _, f := range toRemove {
		path := filepath.Join(stateDir, f)
		if _, err := os.Stat(path); err == nil {
			existingFiles = append(existingFiles, f)
		}
	}

	if len(existingFiles) == 0 {
		fmt.Println("Nothing to reset - VM is already at baseline state.")
		return nil
	}

	fmt.Println("Files to remove:")
	for _, f := range existingFiles {
		fmt.Printf("  - %s\n", f)
	}

	if !resetFlags.confirm {
		fmt.Print("\nProceed? [y/N] ")
		var response string
		if _, err := fmt.Scanln(&response); err != nil {
			// EOF or error reading - treat as "no"
			fmt.Println("Aborted.")
			return nil
		}
		if response != "y" && response != "Y" {
			fmt.Println("Aborted.")
			return nil
		}
	}

	// Remove files
	var removed, failed int
	for _, f := range existingFiles {
		path := filepath.Join(stateDir, f)
		if err := os.Remove(path); err != nil {
			fmt.Printf("  ✗ Failed to remove %s: %v\n", f, err)
			failed++
		} else {
			removed++
		}
	}

	// Clean up empty cloud-init directory
	cloudInitDir := filepath.Join(stateDir, "cloud-init")
	if entries, err := os.ReadDir(cloudInitDir); err == nil && len(entries) == 0 {
		_ = os.Remove(cloudInitDir)
	}

	// If --all, remove the entire directory if empty
	if resetFlags.all {
		if entries, err := os.ReadDir(stateDir); err == nil && len(entries) == 0 {
			_ = os.Remove(stateDir)
			fmt.Printf("\n✓ Removed empty VM directory\n")
		}
	}

	fmt.Printf("\n✓ Reset complete: %d files removed", removed)
	if failed > 0 {
		fmt.Printf(", %d failed", failed)
	}
	fmt.Println()

	if !resetFlags.all {
		fmt.Printf("\nRun 'br start' to create a fresh VM.\n")
	}

	return nil
}
