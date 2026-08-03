//go:build !linux

package imagebuild

import (
	"context"
	"errors"
)

// ErrBakeUnsupported reports that this platform has no native bake mechanic.
var ErrBakeUnsupported = errors.New("the native bake mechanic needs Linux")

// LinuxBakeDeps is the non-Linux counterpart. It returns a set whose Customize
// refuses, rather than an incomplete set: Bake validates its dependencies
// before running any phase, so a nil Customize would report "bake needs a
// Customize" — true, but it would read as a programming mistake rather than as
// this platform not having the mechanic.
//
// The other three operations are real. Fetching and compressing work anywhere,
// and keeping them means the refusal comes from the one thing that genuinely
// cannot run here.
func LinuxBakeDeps(_ string, log func(string)) BakeDeps {
	return BakeDeps{
		Fetch: func(ctx context.Context, r Release, dest string) error {
			return FetchBase(ctx, r, dest, "")
		},
		Run: runHostCommand,
		Customize: func(context.Context, string, []Step) error {
			return ErrBakeUnsupported
		},
		Publish: PublishRename,
		Log:     log,
	}
}
