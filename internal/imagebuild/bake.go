package imagebuild

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// BakeDeps are the operations a bake performs. They are injected so the
// ORCHESTRATION — which phases run, in what order, and what happens when one
// fails — is testable without root, an nbd device, or a network. Those three
// are exactly what made the shell build untestable.
type BakeDeps struct {
	// Fetch acquires the reviewed base image at dest.
	Fetch func(ctx context.Context, r Release, dest string) error
	// Run executes a host command, normally qemu-img.
	Run func(ctx context.Context, argv []string) error
	// Customize applies steps to the image in place. It is the mechanic, and
	// the only part that needs a platform.
	Customize func(ctx context.Context, basePath string, steps []Step) error
	// Publish moves the finished image to its final name.
	Publish func(from, to string) error
	// Log receives one line per phase. Nil discards them.
	Log func(string)
}

// Bake performs a plan.
//
// Phases run in the order Phases() gives and the first failure stops the bake.
// A partially built image that reports success is the failure worth
// preventing: it gets published, boots, and is missing something nobody
// notices until production.
func Bake(ctx context.Context, p BakePlan, deps BakeDeps) error {
	if err := deps.validate(); err != nil {
		return err
	}
	for _, phase := range p.Phases() {
		if deps.Log != nil {
			deps.Log(string(phase))
		}
		if err := deps.run(ctx, p, phase); err != nil {
			return fmt.Errorf("%s: %w", phase, err)
		}
	}
	return nil
}

// run performs one phase.
func (d BakeDeps) run(ctx context.Context, p BakePlan, phase BakePhase) error {
	switch phase {
	case PhaseFetch:
		return d.Fetch(ctx, p.Release, p.BasePath)
	case PhaseResize:
		return d.Run(ctx, p.ResizeArgs())
	case PhaseCustomize:
		return d.Customize(ctx, p.BasePath, p.Recipe.Steps())
	case PhaseCompress:
		return d.Run(ctx, p.CompressArgs())
	case PhasePublish:
		return d.Publish(p.PartialPath, p.OutputPath)
	default:
		return fmt.Errorf("unknown bake phase %q", phase)
	}
}

// validate refuses a dependency set that cannot finish, rather than failing
// partway with an image already written.
func (d BakeDeps) validate() error {
	switch {
	case d.Fetch == nil:
		return errors.New("bake needs a Fetch")
	case d.Run == nil:
		return errors.New("bake needs a Run")
	case d.Customize == nil:
		return errors.New("bake needs a Customize; it is the only part that needs a platform")
	case d.Publish == nil:
		return errors.New("bake needs a Publish")
	}
	return nil
}

// PublishRename is the default Publish: a rename, atomic within a filesystem,
// which is why the partial is written beside the output.
func PublishRename(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return fmt.Errorf("move the finished image into place: %w", err)
	}
	return nil
}
