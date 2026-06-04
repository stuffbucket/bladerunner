package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/cartridge"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/control"
)

var ejectFlags struct {
	disk    string
	force   bool
	timeout time.Duration
}

var ejectCmd = &cobra.Command{
	Use:   "eject [name]",
	Short: "Cleanly power off the active VM (and detach its cartridge)",
	Long: `Gracefully shut the running guest down via the ACPI power button and tear the
VM down — the clean inverse of 'runner boot'. The foreground runner loops the
ACPI request and waits for the guest to power off (up to --timeout), then forces
the stop. For a cartridge boot, the released image is detached on the way out, so
the cartridge is left in a consistent cold-boot state ready to AirDrop.

This is a clean shutdown, not a RAM snapshot: a later 'runner boot' cold-boots.
(For a same-host RAM resume, use 'runner save' + 'runner restore' instead.)

With one slot booted, the name is optional. With several booted slots, name the
one to eject (a cartridge name, a disk name, or "default").`,
	Args: cobra.MaximumNArgs(1),
	RunE: runEject,
}

func init() {
	ejectCmd.Flags().StringVar(&ejectFlags.disk, "disk", "", "Which slot to eject (default: the single booted slot)")
	ejectCmd.Flags().BoolVarP(&ejectFlags.force, "force", "f", false, "Force the stop without waiting the full graceful timeout")
	ejectCmd.Flags().DurationVar(&ejectFlags.timeout, "timeout", ejectTimeoutDuration, "How long to wait for a graceful ACPI shutdown")
}

func runEject(_ *cobra.Command, args []string) error {
	name := ejectFlags.disk
	if name == "" && len(args) == 1 {
		name = args[0]
	}

	baseDir, slotName, err := resolveEjectSlot(name)
	if err != nil {
		return jsonOrError(err)
	}

	client := control.NewClient(baseDir)
	if !client.IsRunning() {
		return jsonOrError(fmt.Errorf("%q is not booted", slotName))
	}

	if !jsonOutput {
		fmt.Printf("%s %s (graceful ACPI shutdown)...\n", subtle("Ejecting"), value(slotName))
	}

	// The server gracefully stops the guest, then exits — releasing and detaching
	// any cartridge. Send the eject message, then wait for the control socket to
	// disappear (the server initiates its own shutdown; we must NOT also StopVM).
	timeoutSeconds := int(ejectFlags.timeout / time.Second)
	if err := client.Eject(ejectFlags.force, timeoutSeconds); err != nil {
		return jsonOrError(fmt.Errorf("eject %q: %w", slotName, err))
	}

	// Allow the graceful shutdown + detach plus a margin over the server-side wait.
	wait := ejectFlags.timeout + ejectWaitMargin
	if !waitForSocketGone(control.SocketPath(baseDir), wait) {
		return jsonOrError(fmt.Errorf("%q did not finish shutting down within %s", slotName, wait))
	}

	if jsonOutput {
		return emitJSON(map[string]string{jsonFieldStatus: "ejected", "name": slotName})
	}
	fmt.Printf("%s Ejected %s\n", success("✓"), value(slotName))
	return nil
}

// ejectWaitMargin is added to the eject timeout when waiting for the control
// socket to disappear, covering VMM teardown + cartridge detach after the guest
// has powered off.
const ejectWaitMargin = 15 * time.Second

// resolveEjectSlot determines which slot to eject. An explicit name selects its
// slot directly (a cartridge under mnt/<name>, a disk under disks/<name>, or the
// flat default). Otherwise it scans for the single booted slot across attached
// cartridges, disk slots, and the flat default: zero booted is an error, more
// than one requires a name.
func resolveEjectSlot(name string) (baseDir, slotName string, err error) {
	if name != "" {
		return ejectSlotDirForName(name), name, nil
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

	// Disk slots under <state>/disks/*.
	disksRoot := filepath.Join(config.DefaultStateDir(), "disks")
	dirs, readErr := os.ReadDir(disksRoot)
	if readErr != nil && !os.IsNotExist(readErr) {
		return "", "", fmt.Errorf("scan disk slots: %w", readErr)
	}
	for _, e := range dirs {
		if !e.IsDir() {
			continue
		}
		slot := filepath.Join(disksRoot, e.Name())
		if control.NewClient(slot).IsRunning() {
			found = append(found, booted{baseDir: slot, name: e.Name()})
		}
	}

	// Attached cartridges under <state>/mnt/*.
	for _, c := range listAttachedCartridges() {
		if c.Booted {
			found = append(found, booted{baseDir: c.Mountpoint, name: c.Name})
		}
	}

	switch len(found) {
	case 0:
		return "", "", fmt.Errorf("no booted VM to eject")
	case 1:
		return found[0].baseDir, found[0].name, nil
	default:
		names := make([]string, 0, len(found))
		for _, b := range found {
			names = append(names, b.name)
		}
		return "", "", fmt.Errorf("multiple VMs booted (%v); name one to eject", names)
	}
}

// ejectSlotDirForName resolves a slot name to its control-socket base dir: an
// attached cartridge's mountpoint wins (it owns a live socket there), else the
// disk slot under disks/<name>, else (for "default") the flat layout.
func ejectSlotDirForName(name string) string {
	if name == "default" {
		return config.DefaultStateDir()
	}
	mp := cartridgeMountpoint(name)
	if cartridge.IsAttached(mp) {
		return mp
	}
	return diskSlotDir(name)
}
