package main

import (
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/instance"
	"github.com/stuffbucket/bladerunner/internal/vmhost"
)

// TestRegistrableSlotNameRefusesAnUnregistrableStateDir pins the fix for a
// defect found by running two VMs at once on real hardware.
//
// The Host derives an instance name with config.InstanceName, which is
// filepath.Base of the state directory and is validated nowhere. The registry,
// however, refuses any name instance.ValidName rejects, and that refusal is
// non-fatal by design — it is logged and the VM keeps running.
//
// Before this check, `br start --state-dir /tmp/vmA` therefore booted a VM that
// never appeared in `br instances` and could not be stopped by name. That was
// survivable while start ran in the foreground; now that the VM runs under a
// detached holder, it leaves an invisible VM the user can only kill by PID.
func TestRegistrableSlotNameRefusesAnUnregistrableStateDir(t *testing.T) {
	cases := []struct {
		name    string
		dir     string
		wantErr bool
	}{
		{"uppercase basename", "/tmp/scratch/vmA", true},
		{"underscore basename", "/tmp/scratch/my_vm", true},
		{"leading dash", "/tmp/scratch/-vm", true},
		{"dot basename", "/tmp/scratch/vm.one", true},
		{"ordinary lowercase", "/tmp/scratch/demo", false},
		{"dashed lowercase", "/tmp/scratch/demo-two", false},
		{"digits", "/tmp/scratch/vm2", false},
		{"empty means the default slot", "", false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			err := registrableSlotName(vmhost.Spec{StateDir: tt.dir})
			if tt.wantErr != (err != nil) {
				t.Fatalf("registrableSlotName(%q) error = %v, wantErr %v", tt.dir, err, tt.wantErr)
			}
			if !tt.wantErr {
				return
			}
			// The error has to be actionable: it must name the directory the
			// user actually passed, so they know which one to rename.
			if !strings.Contains(err.Error(), tt.dir) {
				t.Errorf("error does not name the state dir %q: %v", tt.dir, err)
			}
			if !strings.Contains(err.Error(), "--state-dir") {
				t.Errorf("error does not name the flag at fault: %v", err)
			}
		})
	}
}

// TestRegistrableSlotNameAcceptsAnExplicitSpecName checks the check defers to a
// name the caller set deliberately, rather than re-deriving one from the path.
func TestRegistrableSlotNameAcceptsAnExplicitSpecName(t *testing.T) {
	// An explicit, valid name wins even though the directory basename would be
	// rejected on its own.
	if err := registrableSlotName(vmhost.Spec{Name: "demo", StateDir: "/tmp/scratch/vmA"}); err != nil {
		t.Fatalf("an explicit valid name should be accepted: %v", err)
	}
	// An explicit INVALID name must still be refused, and by the same rule.
	err := registrableSlotName(vmhost.Spec{Name: "vmA", StateDir: "/tmp/scratch/demo"})
	if err == nil {
		t.Fatal("an explicit invalid name should be refused")
	}
	if !strings.Contains(err.Error(), "vmA") {
		t.Errorf("error does not name the offending name: %v", err)
	}
}

// TestRegistrableSlotNameMatchesTheRegistrysOwnRule guards against the two
// rules drifting apart: this check exists only to surface, at start time, the
// refusal the registry would otherwise make silently at publish time.
func TestRegistrableSlotNameMatchesTheRegistrysOwnRule(t *testing.T) {
	for _, name := range []string{"demo", "vmA", "my_vm", "-vm", "vm.one", "vm2", strings.Repeat("a", 200)} {
		fromRegistry := instance.ValidName(name) != nil
		fromCheck := registrableSlotName(vmhost.Spec{Name: name, StateDir: "/tmp/scratch/x"}) != nil
		if fromRegistry != fromCheck {
			t.Errorf("name %q: registry rejects=%v but the start check rejects=%v", name, fromRegistry, fromCheck)
		}
	}
}
