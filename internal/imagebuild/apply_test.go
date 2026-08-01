package imagebuild

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// recordingRunner captures the commands a build would run, so step application
// can be tested without a guest root, root privileges, or chroot.
type recordingRunner struct {
	// commands is every argv received, in order.
	commands [][]string
	// failOn makes Run fail for the first command containing this substring.
	failOn string
}

func (r *recordingRunner) Run(_ context.Context, argv []string) error {
	r.commands = append(r.commands, argv)
	if r.failOn != "" && strings.Contains(strings.Join(argv, " "), r.failOn) {
		return errors.New("synthetic failure")
	}
	return nil
}

// applyToTempRoot runs steps against a fresh temporary root and returns it.
func applyToTempRoot(t *testing.T, steps []Step) (string, *recordingRunner) {
	t.Helper()
	root := t.TempDir()
	runner := &recordingRunner{}
	if _, err := Apply(t.Context(), root, steps, runner); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return root, runner
}

func TestApplyWritesFilesWithTheRequestedMode(t *testing.T) {
	const body = "hello\n"
	root, _ := applyToTempRoot(t, []Step{{
		Kind:    StepWriteFile,
		Desc:    "write a nested file",
		Path:    "/etc/deeply/nested/conf",
		Mode:    0o600,
		Content: body,
	}})

	got, err := os.ReadFile(filepath.Join(root, "etc", "deeply", "nested", "conf"))
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(got) != body {
		t.Errorf("content = %q, want %q", got, body)
	}

	info, err := os.Stat(filepath.Join(root, "etc", "deeply", "nested", "conf"))
	if err != nil {
		t.Fatalf("stat written file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want %v", info.Mode().Perm(), os.FileMode(0o600))
	}
}

// Appending must preserve what the base image already had. Truncating
// /etc/initramfs-tools/modules would drop the distribution's own entries and
// produce an image that boots until it needs one of them.
func TestApplyAppendsWithoutTruncating(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "etc", "initramfs-tools")
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatalf("prepare root: %v", err)
	}
	target := filepath.Join(existing, "modules")
	if err := os.WriteFile(target, []byte("distro_module\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	_, err := Apply(t.Context(), root, []Step{{
		Kind:    StepAppendFile,
		Desc:    "append modules",
		Path:    initramfsModulesPath,
		Mode:    0o644,
		Content: "vhost_vsock\n",
	}}, &recordingRunner{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read appended file: %v", err)
	}
	if want := "distro_module\nvhost_vsock\n"; string(got) != want {
		t.Errorf("content = %q, want %q", got, want)
	}
}

// Appending to a file the base image does not have must still work: the
// destination is created rather than reported as an error.
func TestApplyAppendCreatesAMissingFile(t *testing.T) {
	root, _ := applyToTempRoot(t, []Step{{
		Kind:    StepAppendFile,
		Desc:    "append to a file that does not exist",
		Path:    "/etc/created/on/demand",
		Mode:    0o644,
		Content: "line\n",
	}})

	got, err := os.ReadFile(filepath.Join(root, "etc", "created", "on", "demand"))
	if err != nil {
		t.Fatalf("read created file: %v", err)
	}
	if string(got) != "line\n" {
		t.Errorf("content = %q, want %q", got, "line\n")
	}
}

func TestApplyRunsCommandsInOrder(t *testing.T) {
	_, runner := applyToTempRoot(t, []Step{
		{Kind: StepRun, Desc: "first", Argv: []string{"one"}},
		{Kind: StepRun, Desc: "second", Argv: []string{"two", "arg"}},
	})

	want := [][]string{{"one"}, {"two", "arg"}}
	if !slices.EqualFunc(runner.commands, want, slices.Equal) {
		t.Errorf("commands = %v, want %v", runner.commands, want)
	}
}

// A guest path is not attacker-controlled — recipes are in-tree — but the build
// runs as root on the host, so a destination that resolves anywhere other than
// where the recipe says is both a wrong image and, in the worst case, a write
// onto the host. Both malformed shapes are refused rather than normalised,
// because a silently relocated file produces an image that builds and is wrong.
func TestApplyRefusesAMalformedGuestPath(t *testing.T) {
	for _, tc := range []struct {
		name, path, wantErr string
	}{
		{"traversal above the root", "/../escaped", "not clean"},
		{"traversal mid-path", "/etc/../../escaped", "not clean"},
		{"relative path", "etc/relative", "not absolute"},
		{"trailing slash", "/etc/dir/", "not clean"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			outside := filepath.Join(filepath.Dir(root), "escaped")

			_, err := Apply(t.Context(), root, []Step{{
				Kind:    StepWriteFile,
				Desc:    "malformed destination",
				Path:    tc.path,
				Mode:    0o644,
				Content: "should not be written",
			}}, &recordingRunner{})

			if err == nil {
				t.Fatalf("Apply accepted the malformed guest path %q", tc.path)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
			}
			if _, statErr := os.Stat(outside); statErr == nil {
				t.Errorf("Apply wrote %s, outside the guest root", outside)
			}
		})
	}
}

// Every path the real recipe uses must satisfy that guard. A recipe that cannot
// be applied is a build that fails at the last moment, on the machine that
// already downloaded and mounted an image.
func TestDefaultRecipePathsAreWellFormed(t *testing.T) {
	for _, s := range DefaultRecipe(testVersion).Steps() {
		if s.Kind == StepRun {
			continue
		}
		if err := validateGuestPath(s.Path); err != nil {
			t.Errorf("recipe step %q has an unusable destination: %v", s.Desc, err)
		}
	}
}

// A failed step must stop the build. Continuing would produce an image that is
// missing something, yet reports success and gets published.
func TestApplyStopsAtTheFirstFailedStep(t *testing.T) {
	runner := &recordingRunner{failOn: "boom"}
	_, err := Apply(t.Context(), t.TempDir(), []Step{
		{Kind: StepRun, Desc: "fine", Argv: []string{"ok"}},
		{Kind: StepRun, Desc: "the failing one", Argv: []string{"boom"}},
		{Kind: StepRun, Desc: "never reached", Argv: []string{"after"}},
	}, runner)

	if err == nil {
		t.Fatal("Apply reported success after a step failed")
	}
	if !strings.Contains(err.Error(), "the failing one") {
		t.Errorf("error %q does not name the failing step", err)
	}
	if len(runner.commands) != 2 {
		t.Errorf("ran %d commands, want 2 — the build continued past a failure", len(runner.commands))
	}
}

// An unknown kind means a step was constructed by code this function does not
// understand. Skipping it would silently drop part of the recipe.
func TestApplyRejectsAnUnknownStepKind(t *testing.T) {
	_, err := Apply(t.Context(), t.TempDir(), []Step{
		{Kind: StepKind("teleport"), Desc: "not a real kind"},
	}, &recordingRunner{})
	if err == nil {
		t.Fatal("Apply accepted a step of unknown kind")
	}
	if !strings.Contains(err.Error(), "teleport") {
		t.Errorf("error %q does not name the unknown kind", err)
	}
}

// The whole default recipe must apply cleanly to an empty root, which is the
// closest a test can get to the real build without a guest image.
func TestApplyTheDefaultRecipe(t *testing.T) {
	r := DefaultRecipe(testVersion)
	root, runner := applyToTempRoot(t, r.Steps())

	for _, asset := range r.Assets {
		path, err := filepath.Rel("/", asset.GuestPath)
		if err != nil {
			t.Fatalf("relative path for %s: %v", asset.GuestPath, err)
		}
		got, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Errorf("asset %s was not installed: %v", asset.GuestPath, err)
			continue
		}
		if string(got) != asset.Content {
			t.Errorf("asset %s has unexpected content", asset.GuestPath)
		}
	}

	if len(runner.commands) == 0 {
		t.Error("the default recipe ran no commands")
	}
}
