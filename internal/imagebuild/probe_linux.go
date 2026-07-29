//go:build linux

package imagebuild

import (
	"context"
	"os"
	"os/exec"
	"time"
)

// applianceProbeTool is libguestfs's own end-to-end self test. It launches the
// appliance and runs a command inside it, which is the only honest way to tell
// a working install from a merely present one.
const applianceProbeTool = "libguestfs-test-tool"

// applianceProbeTimeout bounds the launch check. With KVM the appliance boots in
// about a second; under emulation it takes tens of seconds. The bound is
// generous enough for the emulated case and short enough that a hung probe
// cannot stall a build indefinitely.
const applianceProbeTimeout = 90 * time.Second

// loopControlPath is the kernel's loop-device control node. Its presence is the
// cheapest reliable signal that the loop driver is available: a container whose
// kernel lacks the module has no such node, which is exactly the case where the
// native mechanic would fail partway through.
//
// It is a variable rather than a constant so a test can point it at a path it
// controls and exercise both outcomes without depending on the kernel the tests
// happen to run under.
var loopControlPath = "/dev/loop-control"

// loopDeviceAvailable reports whether a loop device can be obtained.
func loopDeviceAvailable() bool {
	f, err := os.Open(loopControlPath)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// applianceUsable reports whether libguestfs can actually boot its appliance
// here, by running libguestfs's own self test rather than trusting that the
// binaries exist. See appliance.go for the settings that invocation needs.
func applianceUsable(ctx context.Context) bool {
	if _, err := exec.LookPath(applianceProbeTool); err != nil {
		return false
	}

	ctx, cancel := context.WithTimeout(ctx, applianceProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, applianceProbeTool)
	cmd.Env = applianceEnv(os.Environ())
	return cmd.Run() == nil
}

// vmRuntimeUsable reports no bladerunner VM runtime: booting a VM needs
// Virtualization.framework, which is macOS-only.
func vmRuntimeUsable() bool { return false }
