// Package imagebuild owns building the pre-baked bladerunner guest image.
//
// It is the single owner of two things that were previously split between
// scripts/build-guest-image.sh and cmd/bladerunner: the build RECIPE (what goes
// into the image) and the build POLICY (which mechanic customizes it, and what
// happens when the preferred one cannot run).
//
// Two mechanics exist because they trade against each other:
//
//   - MethodNative mounts the image on the host and chroots into it. It runs at
//     native speed but needs a Linux host, root, a loop device, and a target
//     architecture matching the host's.
//   - MethodAppliance drives libguestfs, which boots an unprivileged microVM.
//     It needs none of the above and works in a plain container, but on a host
//     without KVM it runs under emulation — roughly an order of magnitude
//     slower on a measured install-and-regenerate-initramfs workload.
//   - MethodVM boots a short-lived bladerunner VM. It is how macOS builds, since
//     a macOS host can neither chroot into a Linux root nor run libguestfs.
//
// Policy is deliberately free of I/O so it can be tested exhaustively without
// root, a loop device, or a hypervisor. Capabilities are gathered by the
// platform-specific probe and passed in.
package imagebuild

import (
	"errors"
	"fmt"
	"strings"
)

// Method identifies the mechanic used to customize the image.
type Method string

const (
	// MethodAuto asks Select to choose, preferring the fastest mechanic that
	// will actually work on this host.
	MethodAuto Method = "auto"
	// MethodNative mounts the image and chroots into it. Linux, root, loop
	// device, same architecture.
	MethodNative Method = "native"
	// MethodAppliance drives libguestfs, which boots an unprivileged microVM.
	MethodAppliance Method = "appliance"
	// MethodVM boots a short-lived bladerunner VM to build inside.
	MethodVM Method = "vm"
)

// Platform names, compared against Capabilities.GOOS.
const (
	osLinux  = "linux"
	osDarwin = "darwin"
)

// ErrUnsupportedPlatform reports a host that has no build mechanic at all.
var ErrUnsupportedPlatform = errors.New("guest image builds are not supported on this platform")

// Capabilities describes what a host can actually do, as established by the
// platform probe. It is a plain value so policy stays testable.
type Capabilities struct {
	// GOOS is the host operating system, as runtime.GOOS reports it.
	GOOS string
	// HostArch is the host architecture in Debian naming (arm64, amd64),
	// which for these two matches runtime.GOARCH.
	HostArch string
	// Elevated reports an effective uid of 0.
	Elevated bool
	// LoopDevice reports that a loop device could be obtained.
	LoopDevice bool
	// ApplianceUsable reports that libguestfs actually launched its appliance,
	// not merely that its binaries are installed. Presence is not function:
	// an installed-but-broken libguestfs is the failure this distinction
	// exists to catch.
	ApplianceUsable bool
	// VMUsable reports a usable bladerunner VM runtime.
	VMUsable bool
}

// Selection is the outcome of policy: the chosen mechanic, plus every reason a
// faster one was rejected. Warnings are returned as data rather than logged
// here so the caller owns presentation and tests can assert on them.
type Selection struct {
	// Method is the mechanic to use.
	Method Method
	// Warnings explains, in order, why any preferred mechanic was skipped.
	// Empty when the fastest mechanic was chosen.
	Warnings []string
}

// Select resolves want into a usable mechanic for targetArch on the host
// described by caps.
//
// An explicit want is never silently substituted: if it cannot run, Select
// returns an error naming every blocking condition. Only MethodAuto falls back,
// and a fallback always carries a reason — a silent degrade would hide a
// misconfigured host behind a build that merely got slower.
func Select(want Method, targetArch string, caps Capabilities) (Selection, error) {
	if err := supportedHost(caps); err != nil {
		return Selection{}, err
	}

	switch want {
	case MethodAuto:
		return selectAuto(targetArch, caps)
	case MethodNative, MethodAppliance, MethodVM:
		if blockers := blockersFor(want, targetArch, caps); len(blockers) > 0 {
			return Selection{}, fmt.Errorf("--method %s was requested but cannot run here: %s",
				want, strings.Join(blockers, "; "))
		}
		return Selection{Method: want}, nil
	default:
		return Selection{}, fmt.Errorf("unknown method %q (expected %s, %s, %s or %s)",
			want, MethodAuto, MethodNative, MethodAppliance, MethodVM)
	}
}

// supportedHost rejects hosts with no mechanic at all. Windows is called out
// explicitly because WSL2 makes it a solved problem rather than a dead end.
func supportedHost(caps Capabilities) error {
	switch caps.GOOS {
	case osLinux, osDarwin:
		return nil
	default:
		return fmt.Errorf("%w: %s has no build mechanic; on Windows build inside WSL2, which is Linux and takes the native path: %w",
			ErrUnsupportedPlatform, caps.GOOS, ErrUnsupportedPlatform)
	}
}

// selectAuto walks the platform's mechanics fastest-first and takes the first
// that is unblocked, carrying forward why the faster ones were skipped.
func selectAuto(targetArch string, caps Capabilities) (Selection, error) {
	var warnings []string
	var refused []string

	for _, m := range preferenceOrder(caps.GOOS) {
		blockers := blockersFor(m, targetArch, caps)
		if len(blockers) == 0 {
			return Selection{Method: m, Warnings: warnings}, nil
		}
		for _, b := range blockers {
			warnings = append(warnings, fmt.Sprintf("%s method unavailable: %s", m, b))
			refused = append(refused, fmt.Sprintf("%s: %s", m, b))
		}
	}

	return Selection{}, fmt.Errorf("no usable build method on this host: %s", strings.Join(refused, "; "))
}

// preferenceOrder lists a platform's mechanics fastest-first.
//
// Linux prefers native because it is roughly an order of magnitude quicker than
// the appliance, and falls back to the appliance because it needs neither root
// nor a loop device. macOS has only the VM: it can neither chroot into a Linux
// root nor run libguestfs.
func preferenceOrder(goos string) []Method {
	if goos == osDarwin {
		return []Method{MethodVM}
	}
	return []Method{MethodNative, MethodAppliance}
}

// blockersFor returns every reason m cannot run, in a stable order. An empty
// result means m is usable.
func blockersFor(m Method, targetArch string, caps Capabilities) []string {
	switch m {
	case MethodNative:
		return nativeBlockers(targetArch, caps)
	case MethodAppliance:
		return applianceBlockers(caps)
	case MethodVM:
		return vmBlockers(caps)
	default:
		return []string{fmt.Sprintf("unknown method %q", m)}
	}
}

// nativeBlockers checks the four conditions the chroot mechanic needs. All are
// reported together so an operator can fix them in one pass.
func nativeBlockers(targetArch string, caps Capabilities) []string {
	var blockers []string
	if caps.GOOS != osLinux {
		blockers = append(blockers, fmt.Sprintf("needs a Linux host to chroot into the guest root, but this host is %s", caps.GOOS))
	}
	if !caps.Elevated {
		blockers = append(blockers, "needs root (euid 0) to mount the image and chroot")
	}
	if !caps.LoopDevice {
		blockers = append(blockers, "no loop device is available to attach the image")
	}
	if targetArch != caps.HostArch {
		blockers = append(blockers, fmt.Sprintf("cross-architecture build (host %s, target %s) and chroot cannot run foreign binaries",
			caps.HostArch, targetArch))
	}
	return blockers
}

// applianceBlockers reports whether libguestfs actually works here.
func applianceBlockers(caps Capabilities) []string {
	if !caps.ApplianceUsable {
		return []string{"libguestfs could not launch its appliance (install libguestfs-tools, and check it starts)"}
	}
	return nil
}

// vmBlockers reports whether a short-lived bladerunner VM can be booted.
func vmBlockers(caps Capabilities) []string {
	// Returning early rather than accumulating avoids a redundant second
	// GOOS test: off macOS the platform is the only blocker worth reporting,
	// and asking about the VM runtime there is meaningless.
	if caps.GOOS != osDarwin {
		return []string{fmt.Sprintf("needs macOS to boot a bladerunner VM, but this host is %s", caps.GOOS)}
	}
	if !caps.VMUsable {
		return []string{"no usable VM runtime (Virtualization.framework needs a signed binary; run make sign)"}
	}
	return nil
}

// shouldProbeAppliance reports whether running the libguestfs launch check can
// still change the outcome.
//
// The check boots a real appliance and costs seconds, so it is worth skipping
// when native is already viable. It is a separate predicate rather than an
// inline condition so it can be tested on any platform: inside Probe its effect
// is invisible off Linux, where applianceUsable always reports false and both
// branches therefore produce the same capabilities.
func shouldProbeAppliance(want Method, targetArch string, caps Capabilities) bool {
	return want == MethodAppliance || len(nativeBlockers(targetArch, caps)) > 0
}
