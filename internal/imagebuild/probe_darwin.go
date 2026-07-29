//go:build darwin

package imagebuild

import "context"

// loopDeviceAvailable reports no loop device: macOS has no Linux loop driver,
// and the native mechanic's mount-and-chroot cannot work here at all.
func loopDeviceAvailable() bool { return false }

// applianceUsable reports no libguestfs. It is a Linux toolchain that boots a
// Linux appliance under qemu; a macOS host builds inside a bladerunner VM
// instead, which fills the same role.
func applianceUsable(context.Context) bool { return false }

// vmRuntimeUsable reports that a bladerunner VM can be booted.
//
// Whether Virtualization.framework will actually start cannot be established
// without attempting a boot: it requires the com.apple.security.virtualization
// entitlement, which is a property of the signed binary rather than something
// the process can interrogate. Rather than guess, this reports true and lets the
// mechanic surface the real failure, which internal/vm already renders as an
// actionable "run make sign" message.
func vmRuntimeUsable() bool { return true }
