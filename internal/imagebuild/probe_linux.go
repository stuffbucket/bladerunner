//go:build linux

package imagebuild

import (
	"context"
	"os"
	"os/exec"
	"time"
)

// loopControlPath is the kernel's loop-device control node. Its presence is the
// cheapest reliable signal that the loop driver is available: a container whose
// kernel lacks the module has no such node, which is exactly the case where the
// native mechanic would fail partway through.
const loopControlPath = "/dev/loop-control"

// applianceProbeTimeout bounds the libguestfs launch check. On a host with KVM
// the appliance boots in about a second; without it, under emulation, it takes
// tens of seconds. The bound is generous enough for the emulated case and short
// enough that a hung probe cannot stall a build indefinitely.
const applianceProbeTimeout = 90 * time.Second

// applianceProbeTool is libguestfs's own end-to-end self test. It launches the
// appliance and runs a command inside it, which is the only honest way to tell a
// working install from a merely present one.
const applianceProbeTool = "libguestfs-test-tool"

// libguestfsForceTCG makes libguestfs pick TCG explicitly.
//
// Without it, libguestfs on aarch64 misparses its own QMP capability probe,
// concludes KVM is enabled, and emits KVM-only qemu flags (gic-version=host,
// -cpu host). qemu then falls back to TCG and rejects those flags, so the
// appliance never boots. Forcing TCG makes the probe and the real run agree.
const libguestfsForceTCG = "LIBGUESTFS_BACKEND_SETTINGS=force_tcg"

// libguestfsDirectBackend avoids libvirt, which is not necessarily configured on
// a build host and adds a second failure surface for no benefit here.
const libguestfsDirectBackend = "LIBGUESTFS_BACKEND=direct"

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
// binaries exist.
func applianceUsable(ctx context.Context) bool {
	if _, err := exec.LookPath(applianceProbeTool); err != nil {
		return false
	}

	ctx, cancel := context.WithTimeout(ctx, applianceProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, applianceProbeTool)
	cmd.Env = append(os.Environ(), libguestfsDirectBackend, libguestfsForceTCG)
	return cmd.Run() == nil
}

// vmRuntimeUsable reports no bladerunner VM runtime: booting a VM needs
// Virtualization.framework, which is macOS-only.
func vmRuntimeUsable() bool { return false }
