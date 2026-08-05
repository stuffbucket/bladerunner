package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/instance"
)

var resetCmd = &cobra.Command{
	Use:   "reset",
	Short: "Reset VM to baseline state",
	Long: `Reset the Bladerunner VM by removing its disk and cloud-init files, allowing a fresh start.
This keeps the base image, SSH keys, and client certificates intact.

Reset acts on the instance selected by --instance (or the single running one),
and refuses to delete the disk of a VM that is still running — stop it first, or
pass --force.

It resets a flat or disk-slot instance only. A cartridge keeps its disk and
state inside its own image, so 'br reset' refuses one: eject it and boot the
shipped .dmg again for a fresh working copy.

Use --full to also remove the base image (will be re-downloaded on next start).
Use --all to remove everything including keys and certificates.`,
	RunE: runReset,
}

var resetFlags struct {
	full    bool
	all     bool
	confirm bool
	force   bool
}

func init() {
	resetCmd.Flags().BoolVar(&resetFlags.full, "full", false, "Also remove the base image")
	resetCmd.Flags().BoolVar(&resetFlags.all, "all", false, "Remove everything (complete reset)")
	resetCmd.Flags().BoolVarP(&resetFlags.confirm, "yes", "y", false, "Skip confirmation prompt")
	resetCmd.Flags().BoolVarP(&resetFlags.force, "force", "f", false, "Reset even if the VM is running (deletes its disk out from under it)")
}

func runReset(_ *cobra.Command, _ []string) error {
	// Reset deletes an instance's disk, so it must target the same instance
	// every other verb does — hard-coding the default state dir would silently
	// wipe the wrong VM on a multi-instance host.
	target, err := resolveInstanceTarget()
	if err != nil {
		return jsonOrError(err)
	}
	return resetInstance(target, resetFlags.force)
}

// resetInstance is runReset with the instance already resolved, so the
// running-VM guard and the deletion below are exercisable without a live
// control socket.
func resetInstance(target resolvedInstance, force bool) error {
	if err := ensureResetSupported(target); err != nil {
		return jsonOrError(err)
	}
	if err := ensureResetSafe(target, force); err != nil {
		return jsonOrError(err)
	}
	stateDir := target.StateDir

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

// ensureResetSupported refuses a kind of instance whose baseline this command
// cannot restore.
//
// resetFileList names the FLAT layout: disk.raw, efi-vars.bin,
// cloud-init/user-data. A cartridge's paths come from cartridge.Opened.ApplyTo
// instead — root.img at the mount root, EFI vars and cloud-init under
// <mount>/state — so not one candidate ever matched and `br reset` on a
// cartridge printed "Nothing to reset - VM is already at baseline state" over a
// VM that was nowhere near baseline.
//
// The fix is a refusal, not a second file list: those files live INSIDE the
// user's cartridge image, and deleting the wrong ones there destroys the
// artifact they would have AirDropped (AGENTS.md section 8). A cartridge has a
// baseline of its own — the shipped .dmg, which every boot re-converts into a
// fresh working copy — so the message points at that instead.
func ensureResetSupported(target resolvedInstance) error {
	if target.Kind != instance.KindCartridge {
		return nil
	}
	name := resetTargetName(target)
	return fmt.Errorf("%q is a cartridge; 'br reset' cannot reset one — its disk, EFI state and cloud-init live inside the cartridge image, not in a host state directory. "+
		"Eject it with 'br eject %s' and boot the shipped .dmg again for a fresh working copy, or rebuild it with 'br disk pack'", name, name)
}

// ensureResetSafe refuses to reset a VM that is still running.
//
// The disk file this deletes is the one the VMM has open: unlinking it under a
// live guest loses everything written since boot and leaves the VM running on
// an inode nothing can reach. --force is offered because the same situation is
// also how a wedged instance is recovered, and it follows `br stop --force`:
// same flag, same shorthand, same "I know, do it anyway" meaning.
func ensureResetSafe(target resolvedInstance, force bool) error {
	if !target.Running {
		return nil
	}
	name := resetTargetName(target)
	if !force {
		return fmt.Errorf("%q is running; stop it first with 'br stop' (or reset it anyway with 'br reset --force')", name)
	}
	if !jsonOutput {
		fmt.Printf("%s %s is running; resetting anyway (--force)\n", warning("!"), value(name))
	}
	return nil
}

// resetTargetName is how the guard names the instance it is protecting.
func resetTargetName(target resolvedInstance) string {
	if name := target.instanceName(); name != "" {
		return name
	}
	return target.StateDir
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
		fmt.Printf("\nRun 'br up' to create a fresh VM.\n")
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
