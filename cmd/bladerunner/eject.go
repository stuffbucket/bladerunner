package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/control"
)

var ejectFlags struct {
	disk string
}

var ejectCmd = &cobra.Command{
	Use:   "eject",
	Short: "Pause and save the active disk into its slot",
	Long: `Pause the running guest, write its RAM into the disk's per-disk state slot,
and tear the VM down without resuming — the inverse of 'runner boot'. The saved
RAM and the slot's disk stay consistent, so a later 'runner boot <name>' restores
the guest exactly where you ejected it.

With one disk booted, --disk is optional. With several booted slots, name the one
to eject with --disk <name>.`,
	RunE: runEject,
}

func init() {
	ejectCmd.Flags().StringVar(&ejectFlags.disk, "disk", "", "Which disk slot to eject (default: the single booted slot)")
}

func runEject(_ *cobra.Command, _ []string) error {
	baseDir, name, err := resolveEjectSlot(ejectFlags.disk)
	if err != nil {
		return jsonOrError(err)
	}

	client := control.NewClient(baseDir)
	if !client.IsRunning() {
		return jsonOrError(fmt.Errorf("disk %q is not booted", name))
	}

	// Keep-paused save: the server writes the state file and skips ResumeVM, so
	// the disk stays consistent with the saved RAM.
	savedPath, err := client.SaveState(true)
	if err != nil {
		return jsonOrError(fmt.Errorf("save disk %q: %w", name, err))
	}

	// Tear the (paused) server down and wait for the socket to disappear. The
	// runner's savedState flag prevents a resume on stop, so disk and saved RAM
	// stay coherent.
	if err := stopAndWait(client, baseDir); err != nil {
		return jsonOrError(fmt.Errorf("stop disk %q: %w", name, err))
	}

	if jsonOutput {
		return emitJSON(map[string]string{jsonFieldStatus: "ejected", "disk": name, "path": savedPath})
	}
	fmt.Printf("%s Ejected %s; saved to %s\n", success("✓"), value(name), value(savedPath))
	return nil
}

// resolveEjectSlot determines which slot to eject. An explicit name selects its
// slot directly. Otherwise it scans <DefaultStateDir>/disks/* (plus the flat
// default layout, for back-compat) for the one booted slot: zero booted is an
// error, more than one requires --disk.
func resolveEjectSlot(name string) (baseDir, slotName string, err error) {
	if name != "" {
		return diskSlotDir(name), name, nil
	}

	type booted struct {
		baseDir string
		name    string
	}
	var found []booted

	// The flat default layout (a plain `runner start`) counts as a booted slot.
	flat := config.DefaultStateDir()
	if control.NewClient(flat).IsRunning() {
		found = append(found, booted{baseDir: flat, name: "default"})
	}

	disksRoot := filepath.Join(config.DefaultStateDir(), "disks")
	entries, readErr := os.ReadDir(disksRoot)
	if readErr != nil && !os.IsNotExist(readErr) {
		return "", "", fmt.Errorf("scan disk slots: %w", readErr)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		slot := filepath.Join(disksRoot, e.Name())
		if control.NewClient(slot).IsRunning() {
			found = append(found, booted{baseDir: slot, name: e.Name()})
		}
	}

	switch len(found) {
	case 0:
		return "", "", fmt.Errorf("no booted disk to eject")
	case 1:
		return found[0].baseDir, found[0].name, nil
	default:
		names := make([]string, 0, len(found))
		for _, b := range found {
			names = append(names, b.name)
		}
		return "", "", fmt.Errorf("multiple disks booted (%v); name one with --disk", names)
	}
}
