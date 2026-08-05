//go:build !linux

package imagebuild

// nativeAttachAvailable reports no block-device attach.
//
// The mechanic mounts a Linux root filesystem and chroots into it, and neither
// is available off Linux — macOS has no loop or nbd driver, and Windows is
// served by WSL2, which is Linux and compiles the other file.
//
// This is one file rather than a darwin and an other, because with a single
// mechanic they had the same answer. They were split when the package
// advertised a libguestfs appliance and a bladerunner VM, so that each platform
// could say no to those differently. Both were stubs and are gone.
func nativeAttachAvailable() bool { return false }
