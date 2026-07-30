//go:build darwin

package imagebuild

import "context"

// nativeAttachAvailable reports no block-device attach: macOS has no Linux loop driver,
// and the native mechanic's mount-and-chroot cannot work here at all.
func nativeAttachAvailable() bool { return false }

// applianceUsable reports no libguestfs. It is a Linux toolchain that boots a
// Linux appliance under qemu; a macOS host builds inside a bladerunner VM
// instead, which fills the same role.
func applianceUsable(context.Context) bool { return false }

// vmRuntimeUsable reports no usable VM runtime, because the VM mechanic is not
// implemented yet.
//
// Virtualization.framework is present on this host, but reporting true here
// made policy auto-select MethodVM on macOS and the command then refuse it, so
// a capability probe succeeded and the build failed immediately afterwards. A
// probe must describe what can actually be executed, not what the platform
// could support in principle. Reported as point 4 of #239.
//
// When the VM mechanic lands, this becomes a real check. Whether
// Virtualization.framework will start cannot be established without attempting
// a boot — it needs the com.apple.security.virtualization entitlement, which is
// a property of the signed binary rather than something the process can
// interrogate — so the honest check will be to attempt it and let internal/vm
// render the "run make sign" failure.
func vmRuntimeUsable() bool { return false }
