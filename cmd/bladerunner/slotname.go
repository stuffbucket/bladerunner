package main

import (
	"fmt"

	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/instance"
	"github.com/stuffbucket/bladerunner/internal/vmhost"
)

// registrableSlotName refuses a start whose instance name the registry would
// not accept.
//
// The Host does not take its name from the spec on a --state-dir boot: it
// derives one with config.InstanceName, which is filepath.Base of the state
// directory and is validated nowhere (buildStartSpec leaves Spec.Name empty on
// purpose, so Spec.validateIdentity never runs instance.ValidName). The
// registry does validate, and rejects anything ValidName rejects — but that
// refusal is deliberately non-fatal: it is logged and the VM keeps running,
// because a registry entry is an optimization for other processes.
//
// The two together mean `br start --state-dir /tmp/vmA` used to boot a VM that
// never appeared in `br instances` and could not be addressed by name. While
// start ran in the foreground that was survivable — the terminal still owned
// the VM. Now that it runs under a detached holder, the same boot strands an
// invisible VM the user can only find with ps and only stop by PID.
//
// So the check happens here, once, before anything is spawned, and it fails
// loudly instead of leaving a warning in a log nobody reads.
func registrableSlotName(spec vmhost.Spec) error {
	name := spec.Name
	if name == "" {
		name = config.InstanceNameFor(spec.StateDir)
	}
	if err := instance.ValidName(name); err != nil {
		return fmt.Errorf(
			"cannot start in %q: bladerunner names the instance after that directory, and %q is not a usable instance name (%w)\n"+
				"  rename the directory, or pass --state-dir with a lowercase name made of letters, digits and dashes\n"+
				"  otherwise the VM would run but never appear in 'br instances', and could not be stopped by name",
			spec.StateDir, name, err)
	}
	return nil
}
