//go:build !linux

package imagebuild

import (
	"context"
	"fmt"
	"runtime"
)

// Options configures a native customize pass.
//
// The type exists on every platform so callers compile everywhere; only the
// mechanic is Linux-only. See the Linux build of this file for what each field
// does.
type Options struct {
	// BaseImage is the qcow2 to customize, in place.
	BaseImage string
	// WorkDir holds the mount point.
	WorkDir string
	// Steps are the actions to apply, normally Recipe.Steps().
	Steps []Step
	// Device is the nbd device to attach to. Empty means the default.
	Device string
	// Log receives progress lines. Nil discards them.
	Log func(string)
}

// Customize reports that the native mechanic cannot run on this platform.
//
// Mounting a Linux root filesystem and chrooting into it are both Linux
// operations. On macOS the VM mechanic is the supported path, and policy in
// Select already refuses to choose native here — this exists so the refusal is
// also a compile-time guarantee rather than only a runtime one.
func Customize(_ context.Context, _ Options) error {
	return fmt.Errorf("%w: the native mechanic needs Linux to mount a guest root and chroot into it, but this host is %s",
		ErrUnsupportedPlatform, runtime.GOOS)
}
