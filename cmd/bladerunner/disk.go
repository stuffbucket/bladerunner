package main

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/disk"
	"github.com/stuffbucket/bladerunner/internal/util"
)

// diskSlotDir returns the per-disk state slot baseDir, isolated from the flat
// default layout: <DefaultStateDir>/disks/<name>. Passing it to config.Default
// roots disk.raw, saved-state.bin, console.log, efivars, cloud-init, oidc, and
// the control socket (via VMDir) inside the slot, so a whole disk+memory slot is
// isolated with zero per-field surgery.
func diskSlotDir(name string) string {
	return filepath.Join(config.DefaultStateDir(), "disks", name)
}

// savedStatePath returns the saved-RAM file path inside a disk's slot.
func savedStatePath(baseDir string) string {
	return filepath.Join(baseDir, "saved-state.bin")
}

var disksCmd = &cobra.Command{
	Use:   "disks",
	Short: "List the disk shelf (available .disk manifests)",
	Long: `List every disk bladerunner knows about: the embedded builtins and any
user disks in ~/.config/bladerunner/disks/*.disk.

Each disk shows its boot mode, origin (builtin/user), and whether its per-disk
state slot holds saved guest RAM ("saved", restorable with 'runner boot') or is
fresh.`,
	RunE: runDisks,
}

// diskReport is the JSON shape for one row of `runner disks`.
type diskReport struct {
	Name          string `json:"name"`
	Description   string `json:"description,omitempty"`
	Mode          string `json:"mode"`
	Origin        string `json:"origin"`
	HasSavedState bool   `json:"has_saved_state"`
	Slot          string `json:"slot"`
}

func runDisks(_ *cobra.Command, _ []string) error {
	cat, err := disk.LoadCatalog()
	if err != nil {
		return jsonOrError(fmt.Errorf("load disk catalog: %w", err))
	}

	entries := cat.List()
	reports := make([]diskReport, 0, len(entries))
	for _, e := range entries {
		slot := diskSlotDir(e.Manifest.Name)
		reports = append(reports, diskReport{
			Name:          e.Manifest.Name,
			Description:   e.Manifest.Description,
			Mode:          e.Manifest.Boot.Mode,
			Origin:        e.Origin,
			HasSavedState: util.FileExists(savedStatePath(slot)),
			Slot:          slot,
		})
	}

	if jsonOutput {
		return emitJSON(reports)
	}

	if len(reports) == 0 {
		fmt.Println(subtle("No disks available."))
		fmt.Printf("Create one with %s\n", command("runner disk new <name>"))
		return nil
	}

	fmt.Println(title("Disk Shelf"))
	fmt.Println()
	for _, r := range reports {
		state := subtle("fresh")
		if r.HasSavedState {
			state = success("saved")
		}
		fmt.Printf("  %s  %s\n", value(r.Name), state)
		if r.Description != "" {
			fmt.Printf("    %s %s\n", key("about:"), r.Description)
		}
		fmt.Printf("    %s  %s   %s %s\n", key("mode:"), r.Mode, key("origin:"), r.Origin)
	}
	fmt.Println()
	fmt.Printf("Boot one with %s\n", command("runner boot <name>"))
	return nil
}
