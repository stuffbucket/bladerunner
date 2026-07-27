package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
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
VM down — the clean inverse of 'br boot'. The foreground runner loops the
ACPI request and waits for the guest to power off (up to --timeout), then forces
the stop. For a cartridge boot, the released image is detached on the way out, so
the cartridge is left in a consistent cold-boot state ready to AirDrop.

This is a clean shutdown, not a RAM snapshot: a later 'br boot' cold-boots.
(For a same-host RAM resume, use 'br save' + 'br restore' instead.)

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
	if name == "" {
		// The global --instance selector (or BLADERUNNER_INSTANCE) names the
		// slot just as well as the positional argument does.
		name = selectedInstanceName()
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
// slot directly (a registered instance, a cartridge under mnt/<name>, a disk
// under disks/<name>, or the flat default). Otherwise it scans for the single
// booted instance across the registry and the legacy layouts: zero booted is an
// error, more than one requires a name.
func resolveEjectSlot(name string) (baseDir, slotName string, err error) {
	scanner := defaultScanner()

	if name != "" {
		if target, rerr := scanner.resolveNamed(name); rerr == nil {
			return target.StateDir, name, nil
		}
		// An unregistered name still addresses its legacy slot, so the caller
		// reports "not booted" rather than "unknown instance".
		return ejectSlotDirForName(name), name, nil
	}

	found := scanner.runningInstances()
	switch len(found) {
	case 0:
		return "", "", fmt.Errorf("no booted VM to eject")
	case 1:
		return found[0].StateDir, ejectSlotLabel(found[0]), nil
	default:
		names := make([]string, 0, len(found))
		for i := range found {
			names = append(names, ejectSlotLabel(found[i]))
		}
		return "", "", fmt.Errorf("multiple VMs booted (%v); name one to eject", names)
	}
}

// ejectSlotLabel is how a booted instance is named in eject's output. The flat
// default has always been called "default" here, whereas the registry records
// it under config.DefaultInstanceName; keep the familiar label.
func ejectSlotLabel(r resolvedInstance) string {
	if r.isDefaultSlot() {
		return defaultSlotAlias
	}
	return r.Name
}

// ejectSlotDirForName resolves a slot name to its control-socket base dir when
// the registry knows nothing about it: an attached cartridge's mountpoint wins
// (it owns a live socket there), else the disk slot under disks/<name>, else
// (for "default") the flat layout.
//
// The cartridge lookup covers both places a mount can be — the private
// <state>/mnt/<name> and the browsable /Volumes/bladerunner-<name> macOS picks
// — because a booted cartridge has not lived at the former since mounting
// became browsable, and `br eject demo` has to keep working either way.
func ejectSlotDirForName(name string) string {
	if name == defaultSlotAlias {
		return config.DefaultStateDir()
	}
	if mp, ok := attachedCartridgeMountpoint(config.DefaultStateDir(), name); ok {
		return mp
	}
	return diskSlotDir(name)
}
