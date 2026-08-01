package imagebuild

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/stuffbucket/bladerunner/internal/util"
)

// guestDirMode is the mode for directories created to hold an asset. It matches
// what the distribution uses for /etc subdirectories.
const guestDirMode = 0o755

// Runner executes one command inside a guest root.
//
// It is an interface because the mechanics differ entirely in how they do this:
// the native mechanic chroots into a mounted filesystem, and a VM mechanic
// sends the command to a booted guest. Apply is the part they share.
type Runner interface {
	// Run executes argv inside the guest root and returns an error if the
	// command could not be started or exited non-zero.
	Run(ctx context.Context, argv []string) error
}

// Skipped records an optional step that failed and was passed over.
type Skipped struct {
	// Step is the step that did not run to completion.
	Step Step
	// Err is why it failed.
	Err error
}

// Apply performs steps against the guest root mounted at rootDir, running any
// commands through run. It returns the optional steps that were skipped.
//
// A required step stops the build at the first failure. A partially applied
// image that reports success is worse than a failed build: it gets published,
// boots, and is missing something that only shows up in production.
//
// An optional step is different in kind. It covers work that depends on
// something outside the distribution's archive, where an outage should not
// block a release. Those failures are returned rather than logged and dropped,
// so the caller can say which parts of the image are absent — an image quietly
// missing a component is the failure this whole package exists to surface.
func Apply(ctx context.Context, rootDir string, steps []Step, run Runner) ([]Skipped, error) {
	var skipped []Skipped
	for i, s := range steps {
		err := applyStep(ctx, rootDir, s, run)
		if err == nil {
			continue
		}
		if s.Optional {
			skipped = append(skipped, Skipped{Step: s, Err: err})
			continue
		}
		return skipped, fmt.Errorf("step %d of %d (%s): %w", i+1, len(steps), s.Desc, err)
	}
	return skipped, nil
}

// applyStep performs a single step.
func applyStep(ctx context.Context, rootDir string, s Step, run Runner) error {
	switch s.Kind {
	case StepRun:
		return run.Run(ctx, s.Argv)
	case StepWriteFile, StepAppendFile:
		return applyFileStep(rootDir, s)
	default:
		return fmt.Errorf("unknown step kind %q", s.Kind)
	}
}

// applyFileStep resolves a guest-side path against the mounted root and writes
// it.
//
// Two layers guard the destination, because the build runs as root on the host
// and a path that escapes the mounted image overwrites the host's own
// filesystem. First the path must be absolute and already clean: anything else
// is a bug in the recipe, and quietly rewriting it would put a file somewhere
// the recipe did not ask for, producing an image that builds and is wrong.
// Then util.SafeJoin re-checks containment as defense in depth.
func applyFileStep(rootDir string, s Step) error {
	if err := validateGuestPath(s.Path); err != nil {
		return err
	}

	target, err := util.SafeJoin(rootDir, s.Path)
	if err != nil {
		return fmt.Errorf("resolve guest path %s: %w", s.Path, err)
	}

	if err := os.MkdirAll(filepath.Dir(target), guestDirMode); err != nil {
		return fmt.Errorf("create parent directory for %s: %w", s.Path, err)
	}

	if s.Kind == StepAppendFile {
		return appendToFile(target, s)
	}
	if err := util.WriteFileAtomic(target, []byte(s.Content), s.Mode); err != nil {
		return fmt.Errorf("write %s: %w", s.Path, err)
	}
	return nil
}

// validateGuestPath rejects a destination that is not an absolute, already-clean
// guest path.
//
// A relative path has no meaning here — there is no working directory inside an
// offline image — and a path carrying `.` or `..` means the recipe and the file
// that actually appears disagree.
func validateGuestPath(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("guest path %q is not absolute", path)
	}
	if filepath.Clean(path) != path {
		return fmt.Errorf("guest path %q is not clean (it would resolve to %q)", path, filepath.Clean(path))
	}
	return nil
}

// appendToFile adds content to the end of an existing file, creating it when
// the base image does not carry one.
//
// This deliberately does not go through WriteFileAtomic: appending is a
// read-modify-write, and reproducing it atomically would mean reading the whole
// file back and rewriting it. Nothing else touches the mounted root while the
// build runs, so a torn write is not reachable here — an interrupted build
// discards the whole image rather than shipping it.
func appendToFile(target string, s Step) error {
	f, err := os.OpenFile(target, os.O_APPEND|os.O_CREATE|os.O_WRONLY, s.Mode)
	if err != nil {
		return fmt.Errorf("open %s for append: %w", s.Path, err)
	}
	if _, err := f.WriteString(s.Content); err != nil {
		_ = f.Close()
		return fmt.Errorf("append to %s: %w", s.Path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", s.Path, err)
	}
	return nil
}
