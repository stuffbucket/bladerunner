// Package imagebuild owns building the pre-baked bladerunner guest image.
//
// It is the single owner of two things that were previously split between
// scripts/build-guest-image.sh and cmd/bladerunner: the build RECIPE (what goes
// into the image) and the question of whether a host can build at all.
//
// There is ONE mechanic. It mounts the image on the host and chroots into it,
// which needs Linux, root, an nbd device, and a target architecture matching the
// host's. That is a narrow set of requirements, and the package used to advertise
// two more mechanics to soften it — a libguestfs appliance and a bladerunner VM.
// Neither was ever written. Policy could select either one and the bake then ran
// the chroot regardless, so `--method appliance` quietly did the opposite of what
// it said. Both are gone; what remains is what runs.
//
// Every other way to build is a way to get a Linux box, not a way to build. On a
// Mac, colima, lima, UTM and Docker Desktop all supply one, and the bake inside
// it is this ordinary Linux bake. Building a disk from NOTHING — booting an
// installer against a blank disk — is a genuinely different capability and is
// tracked separately rather than stubbed here.
//
// The host check is deliberately free of I/O so it can be tested exhaustively
// without root or an nbd device. Capabilities are gathered by the
// platform-specific probe and passed in.
package imagebuild

import (
	"errors"
	"fmt"
	"strings"
)

// osLinux is the only platform with a build mechanic, compared against
// Capabilities.GOOS.
const osLinux = "linux"

// ErrUnsupportedHost reports a host that cannot run a bake.
var ErrUnsupportedHost = errors.New("this host cannot build a guest image")

// Capabilities describes what a host can actually do, as established by the
// platform probe. It is a plain value so the host check stays testable.
type Capabilities struct {
	// GOOS is the host operating system, as runtime.GOOS reports it.
	GOOS string
	// HostArch is the host architecture in Debian naming (arm64, amd64),
	// which for these two matches runtime.GOARCH.
	HostArch string
	// Elevated reports an effective uid of 0.
	Elevated bool
	// NativeAttach reports that the host can attach a guest image as a block
	// device by the means the mechanic actually uses. That is the nbd path,
	// not a loop device: probing the wrong one accepts hosts the build then
	// fails on.
	NativeAttach bool
}

// CheckHost reports whether caps can build targetArch, naming every blocking
// condition at once.
//
// All blockers are reported together rather than one at a time so an operator
// fixes them in a single pass instead of rediscovering the next one on each
// attempt. The message names what to do instead, because the most common reader
// of this error is on a Mac, where the answer is a Linux VM they probably
// already have.
func CheckHost(targetArch string, caps Capabilities) error {
	blockers := hostBlockers(targetArch, caps)
	if len(blockers) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s; build on a Linux host — a VM (colima, lima, UTM) or WSL2 counts — or use the published image from the guest-image-latest release",
		ErrUnsupportedHost, strings.Join(blockers, "; "))
}

// hostBlockers returns every reason this host cannot bake, in a stable order.
// An empty result means it can.
//
// Off Linux the platform is the ONLY blocker reported. The other three would all
// be true as well, but they are consequences rather than causes: telling a Mac
// user they need root invites them to try sudo, which cannot help. On Linux the
// remaining conditions are each independently fixable, so they are reported
// together and the operator fixes them in one pass.
func hostBlockers(targetArch string, caps Capabilities) []string {
	if caps.GOOS != osLinux {
		return []string{fmt.Sprintf("needs a Linux host to mount the guest root and chroot into it, but this host is %s", caps.GOOS)}
	}

	var blockers []string
	if !caps.Elevated {
		blockers = append(blockers, "needs root (euid 0) to mount the image and chroot")
	}
	if !caps.NativeAttach {
		blockers = append(blockers, "cannot attach the image as a block device (needs qemu-nbd and an nbd device)")
	}
	if targetArch != caps.HostArch {
		blockers = append(blockers, fmt.Sprintf("cross-architecture build (host %s, target %s) and chroot cannot run foreign binaries",
			caps.HostArch, targetArch))
	}
	return blockers
}
