//go:build darwin

package main

import (
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/instance"
)

// The menubar used to read the default state dir directly, so it reported on
// the default VM no matter which instance was actually running. Its own
// cartridge watcher spawns NAMED holders, so the common cartridge flow left it
// showing "Stopped" — and offering Start, which would have booted a second VM
// beside the cartridge the user had just inserted.
//
// These tests pin the three cases: nothing running, exactly one, several.

func TestResolveMenubarTargetWithNothingRunning(t *testing.T) {
	root := t.TempDir()
	got := resolveMenubarTarget(testScanner(root))

	if got.ambiguous {
		t.Error("ambiguous = true, want false when nothing is running")
	}
	if got.stateDir != root {
		t.Errorf("stateDir = %q, want the default root %q", got.stateDir, root)
	}
}

func TestResolveMenubarTargetFollowsTheSingleRunningInstance(t *testing.T) {
	root := t.TempDir()
	slot := makeDiskSlot(t, root, "demo")
	register(t, root, instance.Entry{Name: "demo", Kind: instance.KindDisk, StateDir: slot})

	got := resolveMenubarTarget(testScanner(root, slot))

	if got.ambiguous {
		t.Fatal("ambiguous = true, want false with exactly one instance running")
	}
	if got.stateDir != slot {
		t.Errorf("stateDir = %q, want the running instance %q", got.stateDir, slot)
	}
	if got.name != "demo" {
		t.Errorf("name = %q, want demo", got.name)
	}
	if got.isDefault {
		t.Error("isDefault = true, want false for a named disk slot")
	}
}

func TestResolveMenubarTargetRefusesToChooseAmongSeveral(t *testing.T) {
	root := t.TempDir()
	slot := makeDiskSlot(t, root, "demo")
	register(t, root, instance.Entry{Name: "demo", Kind: instance.KindDisk, StateDir: slot})

	// The flat default AND the disk slot both answer.
	got := resolveMenubarTarget(testScanner(root, root, slot))

	if !got.ambiguous {
		t.Fatal("ambiguous = false, want true with two instances running")
	}
	if got.stateDir != "" {
		t.Errorf("stateDir = %q, want empty: the menubar must not pick one", got.stateDir)
	}
}

func TestStatusTitleNamesANonDefaultInstance(t *testing.T) {
	named := menubarTarget{stateDir: "/x/demo", name: "demo"}
	got := statusTitle(vmHealthy, "", false, named)
	if !strings.Contains(got, "demo") {
		t.Errorf("statusTitle = %q, want it to name the instance it is reporting on", got)
	}

	// The single-VM install must read exactly as it always did.
	def := menubarTarget{stateDir: "/x", name: "bladerunner", isDefault: true}
	if got := statusTitle(vmHealthy, "", false, def); got != "Running — healthy" {
		t.Errorf("statusTitle for the default instance = %q, want the unchanged wording", got)
	}
}

func TestStatusTitleSaysItCannotChoose(t *testing.T) {
	got := statusTitle(vmAmbiguous, "", false, menubarTarget{ambiguous: true})
	if !strings.Contains(strings.ToLower(got), "several") {
		t.Errorf("statusTitle = %q, want it to say why it cannot report on one VM", got)
	}
}

func TestEnablementForAmbiguousDisablesEveryAction(t *testing.T) {
	// Every one of these shells out to `br <verb>` with no --instance. With
	// several VMs up, br itself refuses; the menubar must not present the rows
	// as though a click would do something.
	en := enablementFor(vmAmbiguous, false)
	if en.start || en.stop || en.reconnect || en.restart || en.web || en.shell {
		t.Errorf("enablementFor(vmAmbiguous) = %+v, want every action disabled", en)
	}

	// StartOnFirstAction must not re-enable Web/Shell either.
	if en := enablementFor(vmAmbiguous, true); en.web || en.shell {
		t.Errorf("enablementFor(vmAmbiguous, firstAction) = %+v, want web/shell disabled", en)
	}
}
