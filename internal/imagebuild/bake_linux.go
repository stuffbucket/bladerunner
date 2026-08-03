//go:build linux

package imagebuild

import (
	"context"
)

// LinuxBakeDeps wires a bake to the real operations on Linux.
//
// It is a constructor rather than a set of defaults on BakeDeps because the
// mechanic needs a work directory to mount in, and a default that reached for
// one itself would decide where large files land on behalf of its caller.
func LinuxBakeDeps(workDir string, log func(string)) BakeDeps {
	return BakeDeps{
		Fetch: func(ctx context.Context, r Release, dest string) error {
			// The empty baseURL means the real Debian mirror; only tests pass
			// anything else, and a bake must not be able to.
			return FetchBase(ctx, r, dest, "")
		},
		Run: runHostCommand,
		Customize: func(ctx context.Context, basePath string, steps []Step) error {
			return Customize(ctx, Options{
				BaseImage: basePath,
				WorkDir:   workDir,
				Steps:     steps,
				Log:       log,
			})
		},
		Publish: PublishRename,
		Log:     log,
	}
}
