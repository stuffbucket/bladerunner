package imagebuild

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Mechanic applies steps to an image in place and reports the optional steps it
// passed over. It is the ONE part of a bake that needs a platform, and the one
// part there is currently only one of.
//
// It is a named type so a bake takes its mechanic as an argument rather than
// finding one for itself. Today the only implementation mounts the image and
// chroots into it, so there is nothing to choose between — but the alternative
// shape, where the mechanic is selected by build tag inside the package, is how
// this code came to advertise two mechanics that were never written. A
// dependency that is passed in cannot be claimed without being supplied.
//
// The skipped steps are RETURNED rather than logged, because the caller is the
// only frame that can act on them. A mechanic that logged them left `br disk
// bake` printing a tick over an image with no web UI in it (#265).
type Mechanic func(ctx context.Context, basePath string, steps []Step) ([]Skipped, error)

// BakeDeps are the operations a bake performs. They are injected so the
// ORCHESTRATION — which phases run, in what order, and what happens when one
// fails — is testable without root, an nbd device, or a network. Those three
// are exactly what made the shell build untestable.
type BakeDeps struct {
	// Fetch acquires the reviewed base image at dest.
	Fetch func(ctx context.Context, r Release, dest string) error
	// Run executes a host command, normally qemu-img.
	Run func(ctx context.Context, argv []string) error
	// Customize applies steps to the image in place and reports the optional
	// ones it skipped.
	Customize Mechanic
	// Publish moves the finished image to its final name.
	Publish func(from, to string) error
	// Log receives one line per phase. Nil discards them.
	Log func(string)
}

// NewBakeDeps assembles the operations of a bake around a mechanic.
//
// Everything except the mechanic works on any platform — fetching, running
// qemu-img and renaming a file are not Linux-specific — so they are supplied
// here once rather than duplicated per platform. HostMechanic provides the
// remaining piece, or explains why this host has none.
func NewBakeDeps(customize Mechanic, log func(string)) BakeDeps {
	return BakeDeps{
		Fetch: func(ctx context.Context, r Release, dest string) error {
			// The empty baseURL means the real Debian mirror; only tests pass
			// anything else, and a bake must not be able to.
			return FetchBase(ctx, r, dest, "")
		},
		Run:       runHostCommand,
		Customize: customize,
		Publish:   PublishRename,
		Log:       log,
	}
}

// Bake performs a plan and reports the optional steps it skipped.
//
// Phases run in the order Phases() gives and the first failure stops the bake.
// A partially built image that reports success is the failure worth
// preventing: it gets published, boots, and is missing something nobody
// notices until production.
//
// An optional step that failed is the quieter version of that same failure, so
// the skipped list comes back even when the bake succeeds — and even when it
// does not, because a step skipped before an unrelated failure is still part of
// what happened. The caller must say so; nothing below this frame can.
func Bake(ctx context.Context, p BakePlan, deps BakeDeps) ([]Skipped, error) {
	if err := deps.validate(); err != nil {
		return nil, err
	}
	// qemu-img does not create parent directories, so the compress phase would
	// fail writing the partial — several minutes and several hundred megabytes
	// after a mistake that was knowable before any of it started.
	if err := os.MkdirAll(filepath.Dir(p.OutputPath), guestDirMode); err != nil {
		return nil, fmt.Errorf("create the output directory for %s: %w", p.OutputPath, err)
	}
	var skipped []Skipped
	for _, phase := range p.Phases() {
		if deps.Log != nil {
			deps.Log(string(phase))
		}
		phaseSkipped, err := deps.run(ctx, p, phase)
		skipped = append(skipped, phaseSkipped...)
		if err != nil {
			return skipped, fmt.Errorf("%s: %w", phase, err)
		}
	}
	return skipped, nil
}

// run performs one phase, reporting any optional steps it passed over.
func (d BakeDeps) run(ctx context.Context, p BakePlan, phase BakePhase) ([]Skipped, error) {
	switch phase {
	case PhaseFetch:
		return nil, d.Fetch(ctx, p.Release, p.BasePath)
	case PhaseResize:
		return nil, d.Run(ctx, p.ResizeArgs())
	case PhaseCustomize:
		return d.Customize(ctx, p.BasePath, p.Recipe.Steps())
	case PhaseCompress:
		return nil, d.Run(ctx, p.CompressArgs())
	case PhasePublish:
		return nil, d.Publish(p.PartialPath, p.OutputPath)
	default:
		return nil, fmt.Errorf("unknown bake phase %q", phase)
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

// runHostCommand executes argv and folds its output into the error, so a
// qemu-img failure says what qemu-img said rather than only its exit status.
func runHostCommand(ctx context.Context, argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("no command to run")
	}
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", argv[0], err, out)
	}
	return nil
}
