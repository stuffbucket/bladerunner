//go:build darwin

package vm

import (
	"context"
	"testing"
	"time"

	"github.com/stuffbucket/bladerunner/internal/config"
)

// TestEjectRequiresStartedVM verifies Eject's precondition: with no started VM,
// it returns an error rather than dereferencing a nil VZ handle. The full
// ACPI->stopped state machine is exercised by the gated cartridge integration
// path (a real VM is required to drive vz.VirtualMachineState transitions).
func TestEjectRequiresStartedVM(t *testing.T) {
	r := &Runner{cfg: &config.Config{}}
	err := r.Eject(context.Background(), time.Second, false)
	if err == nil {
		t.Fatal("Eject on an unstarted VM must return an error")
	}
}
