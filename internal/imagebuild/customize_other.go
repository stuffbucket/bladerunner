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

// Customize reports that the mechanic cannot run on this platform.
//
// Mounting a Linux root filesystem and chrooting into it are both Linux
// operations. CheckHost already refuses this host before a bake starts — this
// exists so the refusal is a compile-time guarantee too, rather than only a
// runtime one.
//
// The skipped-step return keeps the signature identical to the Linux build, so
// a caller threading the skipped list compiles on every platform.
func Customize(_ context.Context, _ Options) ([]Skipped, error) {
	return nil, fmt.Errorf("%w: the mechanic needs Linux to mount a guest root and chroot into it, but this host is %s",
		ErrUnsupportedHost, runtime.GOOS)
}
