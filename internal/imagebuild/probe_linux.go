//go:build linux

package imagebuild

import (
	"context"
	"os"
	"os/exec"
	"time"
)

// nbdDevicePath is the first network-block-device node. The native mechanic is
// executed by the build script's qemu-nbd path, which connects the image to
// this device, so its presence is what makes that path viable.
//
// It is a variable so a test can point it at a path it controls and exercise
// both outcomes without depending on the kernel the tests run under.
var nbdDevicePath = "/dev/nbd0"

// haveQemuNBD reports whether the qemu-nbd binary is present. A variable for
// the same reason.
var haveQemuNBD = func() bool {
	_, err := exec.LookPath("qemu-nbd")
	return err == nil
}

// applianceProbeTool is libguestfs's own end-to-end self test. It launches the
// appliance and runs a command inside it, which is the only honest way to tell
// a working install from a merely present one.
const applianceProbeTool = "libguestfs-test-tool"

// applianceProbeTimeout bounds the launch check. With KVM the appliance boots in
// about a second; under emulation it takes tens of seconds. The bound is
// generous enough for the emulated case and short enough that a hung probe
// cannot stall a build indefinitely.
const applianceProbeTimeout = 90 * time.Second

// nativeAttachAvailable reports whether the image can be attached as a block
// device by the means the native mechanic actually uses.
//
// It deliberately checks the nbd device and qemu-nbd rather than a loop device.
// The two are not interchangeable, and this project measured a host where they
// disagreed: a container exposing /dev/loop-control could not load the nbd
// module at all. Probing the loop device there would have accepted a host on
// which the build then fails part-way through, after downloading and resizing
// the image.
//
// The device node only exists once the nbd module is loaded. The build script
// loads it itself, so a host that could run the build may still report false
// here. That is the safe direction to be wrong in: it costs a fallback to the
// slower appliance, not a failed build.
func nativeAttachAvailable() bool {
	if !haveQemuNBD() {
		return false
	}
	f, err := os.Open(nbdDevicePath)
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
	cmd.Env = ApplianceEnv(os.Environ())
	return cmd.Run() == nil
}

// vmRuntimeUsable reports no bladerunner VM runtime: booting a VM needs
// Virtualization.framework, which is macOS-only.
func vmRuntimeUsable() bool { return false }
