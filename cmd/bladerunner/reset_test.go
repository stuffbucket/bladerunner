package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/instance"
)

// resetFixture stages a state dir holding the files a reset would delete and
// returns it along with the disk path the guard exists to protect.
func resetFixture(t *testing.T) (stateDir, disk string) {
	t.Helper()
	stateDir = t.TempDir()
	disk = filepath.Join(stateDir, "disk.raw")
	for _, name := range []string{"disk.raw", "efi-vars.bin", "console.log"} {
		if err := os.WriteFile(filepath.Join(stateDir, name), []byte("live-bytes"), 0o600); err != nil {
			t.Fatalf("stage %s: %v", name, err)
		}
	}
	return stateDir, disk
}

// withResetFlags installs the non-interactive flag set for a test and restores
// whatever was there (the flags are package globals bound by cobra).
func withResetFlags(t *testing.T) {
	t.Helper()
	saved := resetFlags
	resetFlags.confirm = true
	resetFlags.full = false
	resetFlags.all = false
	resetFlags.force = false
	t.Cleanup(func() { resetFlags = saved })
}

// runningTarget describes a live instance rooted at stateDir.
func runningTarget(stateDir string, running bool) resolvedInstance {
	return resolvedInstance{
		Name:     "demo",
		Kind:     instance.KindFlat,
		StateDir: stateDir,
		Running:  running,
		Explicit: true,
	}
}

// TestResetRefusesARunningInstance is the data-loss regression: `br reset` used
// to delete the disk with no liveness check at all, so it would unlink the image
// the VMM had open — losing everything the guest had written and leaving the VM
// running on an unreachable inode.
func TestResetRefusesARunningInstance(t *testing.T) {
	withResetFlags(t)
	stateDir, disk := resetFixture(t)

	err := resetInstance(runningTarget(stateDir, true), false)
	if err == nil {
		t.Fatal("reset of a running instance was allowed")
	}
	// The refusal has to name the instance and the way out.
	for _, want := range []string{"demo", "running", "br stop", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if _, statErr := os.Stat(disk); statErr != nil {
		t.Fatalf("the running VM's disk was deleted: %v", statErr)
	}
}

// With nothing running, reset does exactly what it always did.
func TestResetProceedsWhenNothingIsRunning(t *testing.T) {
	withResetFlags(t)
	stateDir, disk := resetFixture(t)

	if err := resetInstance(runningTarget(stateDir, false), false); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if _, err := os.Stat(disk); !os.IsNotExist(err) {
		t.Fatalf("disk survived a reset of a stopped instance: %v", err)
	}
}

// --force is the escape hatch, spelled as `br stop --force` spells it.
func TestResetForceOverridesTheRunningGuard(t *testing.T) {
	withResetFlags(t)
	stateDir, disk := resetFixture(t)

	if err := resetInstance(runningTarget(stateDir, true), true); err != nil {
		t.Fatalf("forced reset: %v", err)
	}
	if _, err := os.Stat(disk); !os.IsNotExist(err) {
		t.Fatalf("--force did not reset the instance: %v", err)
	}
}

// Reset targets the instance it was pointed at, not the default state dir: on a
// multi-instance host the two differ, and deleting the wrong one is silent.
func TestResetTargetsTheResolvedInstanceDirectory(t *testing.T) {
	withResetFlags(t)
	selected, selectedDisk := resetFixture(t)
	_, otherDisk := resetFixture(t)

	if err := resetInstance(runningTarget(selected, false), false); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if _, err := os.Stat(selectedDisk); !os.IsNotExist(err) {
		t.Fatalf("the selected instance was not reset: %v", err)
	}
	if _, err := os.Stat(otherDisk); err != nil {
		t.Fatalf("an unrelated instance was reset too: %v", err)
	}
}

// A reset flag set must not have to be reconstructed to name the target: the
// guard falls back to the state dir when the instance carries no name.
func TestResetTargetNameFallsBackToTheStateDir(t *testing.T) {
	target := resolvedInstance{StateDir: "/state/instances/demo"}
	if got := resetTargetName(target); got == "" {
		t.Fatal("an unnamed instance must still be nameable in an error")
	}
}
