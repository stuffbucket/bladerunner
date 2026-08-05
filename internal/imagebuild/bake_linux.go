//go:build linux

package imagebuild

import (
	"context"
)

// HostMechanic returns the mechanic this host can run.
//
// The error is the whole point of returning one: a caller cannot get a mechanic
// that does not exist, so a platform without one cannot start a bake that later
// fails inside it. workDir is a parameter because the mechanic mounts there, and
// a default reached for internally would decide where large files land on behalf
// of its caller.
func HostMechanic(workDir string, log func(string)) (Mechanic, error) {
	return func(ctx context.Context, basePath string, steps []Step) error {
		return Customize(ctx, Options{
			BaseImage: basePath,
			WorkDir:   workDir,
			Steps:     steps,
			Log:       log,
		})
	}, nil
}
