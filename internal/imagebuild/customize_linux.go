//go:build linux

package imagebuild

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// defaultNBDDevice is the block device a build attaches the guest image to.
const defaultNBDDevice = "/dev/nbd0"

// Options configures a native customize pass.
type Options struct {
	// BaseImage is the qcow2 to customize. It is modified in place, so the
	// caller owns making a copy of anything it wants to keep.
	BaseImage string
	// WorkDir holds the mount point. It must already exist and is not removed:
	// it may be a shared workspace the caller owns.
	WorkDir string
	// Steps are the actions to apply, normally Recipe.Steps(). The recipe is
	// not passed directly because the mechanic's job is to perform steps, and
	// carrying both would leave two ways to say the same thing.
	Steps []Step
	// Device is the nbd device to attach to. Empty means the default.
	Device string
	// Log receives progress lines. Nil discards them.
	Log func(string)
}

// Customize applies steps to the image at opts.BaseImage, in place.
//
// The image is attached to an nbd device, its root filesystem mounted, and the
// steps applied through a chroot. This is the fast mechanic: it needs Linux,
// root, and a target architecture matching the host's, and in exchange it runs
// roughly an order of magnitude quicker than the libguestfs appliance on a host
// without KVM.
func Customize(ctx context.Context, opts Options) (err error) {
	if opts.BaseImage == "" {
		return errors.New("no base image to customize")
	}
	if _, statErr := os.Stat(opts.BaseImage); statErr != nil {
		return fmt.Errorf("base image %s is not readable: %w", opts.BaseImage, statErr)
	}
	if opts.WorkDir == "" {
		return errors.New("no work directory for the mount point")
	}
	if len(opts.Steps) == 0 {
		return errors.New("no build steps to apply")
	}

	device := opts.Device
	if device == "" {
		device = defaultNBDDevice
	}
	logf := opts.Log
	if logf == nil {
		logf = func(string) {}
	}

	logf(fmt.Sprintf("attaching %s to %s", opts.BaseImage, device))
	mount, err := attachImage(ctx, opts.BaseImage, opts.WorkDir, device)
	if err != nil {
		return fmt.Errorf("attach the guest image: %w", err)
	}

	// Teardown is deferred rather than called at the end, so a failure part-way
	// through the build still detaches the device. Leaving it connected makes
	// the image unreadable and blocks the next build on the machine.
	defer func() {
		logf("detaching the guest image")
		if closeErr := mount.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("detach the guest image: %w", closeErr))
		}
	}()

	logf(fmt.Sprintf("applying %d build steps to %s", len(opts.Steps), mount.Root))
	runner := chrootRunner{root: mount.Root, log: func(line string) { logf("  " + line) }}
	if err := Apply(ctx, mount.Root, opts.Steps, runner); err != nil {
		return fmt.Errorf("apply the build steps: %w", err)
	}

	// The image must be flushed before the device is detached, or the compress
	// step that follows reads a qcow2 the kernel has not finished writing.
	logf("flushing the guest filesystem")
	syncFilesystems()
	return nil
}
