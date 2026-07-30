package imagebuild

import (
	"slices"
	"strings"
	"testing"
)

// testVersion is an arbitrary but fixed build stamp, so step assertions never
// depend on the day the test runs.
const testVersion = "2026.07.29"

// indexOfRun returns the index of the first Run step whose command line contains
// every one of substrs, or -1. Ordering assertions compare these indices.
func indexOfRun(steps []Step, substrs ...string) int {
	for i, s := range steps {
		if s.Kind != StepRun {
			continue
		}
		line := strings.Join(s.Argv, " ")
		if !slices.ContainsFunc(substrs, func(sub string) bool { return !strings.Contains(line, sub) }) {
			return i
		}
	}
	return -1
}

// indexOfFile returns the index of the first file step targeting path, or -1.
func indexOfFile(steps []Step, path string) int {
	for i, s := range steps {
		if s.Kind != StepRun && s.Path == path {
			return i
		}
	}
	return -1
}

// The recipe must actually drive the build. Every package it declares has to
// reach an install command, or the recipe is decoration and the real package set
// still lives somewhere else — the drift this package exists to end.
func TestStepsInstallEveryRecipePackage(t *testing.T) {
	r := DefaultRecipe(testVersion)
	steps := r.Steps()

	i := indexOfRun(steps, "apt-get", "install")
	if i < 0 {
		t.Fatalf("no apt-get install step in %d steps", len(steps))
	}
	argv := steps[i].Argv

	for _, pkg := range r.Packages {
		if !slices.Contains(argv, pkg) {
			t.Errorf("package %q from the recipe is not installed by %v", pkg, argv)
		}
	}
	if !slices.Contains(argv, "-y") {
		t.Errorf("apt-get install is not non-interactive: %v", argv)
	}
}

// An asset that is copied in but never enabled, or enabled but never copied in,
// produces an image that is broken in a way only a boot reveals.
func TestStepsInstallEveryAssetAndEnableEveryUnit(t *testing.T) {
	r := DefaultRecipe(testVersion)
	steps := r.Steps()

	for _, asset := range r.Assets {
		i := indexOfFile(steps, asset.GuestPath)
		if i < 0 {
			t.Fatalf("asset %q is never installed", asset.GuestPath)
		}
		if got := steps[i].Mode; got != asset.Mode {
			t.Errorf("asset %q installed with mode %v, recipe says %v", asset.GuestPath, got, asset.Mode)
		}
		if steps[i].Content != asset.Content {
			t.Errorf("asset %q installed with content that differs from the recipe", asset.GuestPath)
		}
	}

	for _, unit := range r.EnableUnits {
		if indexOfRun(steps, "systemctl", "enable", unit) < 0 {
			t.Errorf("unit %q from the recipe is never enabled", unit)
		}
	}
}

// The version stamp is how a running guest reports which image it came from.
func TestStepsWriteTheVersionStamp(t *testing.T) {
	r := DefaultRecipe(testVersion)
	steps := r.Steps()

	i := indexOfFile(steps, r.VersionPath)
	if i < 0 {
		t.Fatalf("version stamp %q is never written", r.VersionPath)
	}
	if got := strings.TrimSpace(steps[i].Content); got != testVersion {
		t.Errorf("version stamp content = %q, want %q", got, testVersion)
	}
}

// Ordering is not cosmetic here: each of these pairs produces a silently broken
// image when inverted, and none of them fails the build.
func TestStepsOrderDependenciesCorrectly(t *testing.T) {
	r := DefaultRecipe(testVersion)
	steps := r.Steps()

	install := indexOfRun(steps, "apt-get", "install")
	chrony := indexOfFile(steps, "/etc/chrony/chrony.conf")
	modules := indexOfFile(steps, initramfsModulesPath)
	regen := indexOfRun(steps, "update-initramfs")
	update := indexOfRun(steps, "apt-get", "update")

	for _, c := range []struct {
		name       string
		first, sec int
		why        string
	}{
		{"apt update before install", update, install, "apt-get install cannot resolve packages without a package list"},
		{"install before chrony.conf", install, chrony, "the chrony package creates /etc/chrony, so writing the conf first loses it"},
		{"modules before update-initramfs", modules, regen, "an initramfs regenerated before the modules are listed omits vsock"},
	} {
		if c.first < 0 || c.sec < 0 {
			t.Errorf("%s: a required step is missing (indices %d, %d)", c.name, c.first, c.sec)
			continue
		}
		if c.first >= c.sec {
			t.Errorf("%s: step %d runs at or after step %d — %s", c.name, c.first, c.sec, c.why)
		}
	}

	// Every unit must be enabled only once its package is installed; systemctl
	// enable on a unit that does not exist yet fails the build.
	for _, unit := range r.EnableUnits {
		if i := indexOfRun(steps, "systemctl", "enable", unit); i >= 0 && i < install {
			t.Errorf("unit %q is enabled at step %d, before packages are installed at step %d", unit, i, install)
		}
	}
}

// The build leaves apt scaffolding behind unless it is removed last. A baked
// image carrying a populated apt cache is both larger and stale on first boot.
func TestStepsCleanUpAptStateLast(t *testing.T) {
	steps := DefaultRecipe(testVersion).Steps()

	clean := indexOfRun(steps, "apt-get", "clean")
	if clean < 0 {
		t.Fatal("the build never runs apt-get clean")
	}
	if indexOfRun(steps, "update-initramfs") > clean {
		t.Error("update-initramfs runs after apt-get clean; it needs the packages still cached")
	}
	if indexOfFile(steps, aptRetriesConfPath) < 0 {
		t.Errorf("no step writes %s, so a transient mirror reset fails the whole build", aptRetriesConfPath)
	}
}

// A step with no description cannot be reported usefully when it fails, and the
// mechanic logs one line per step.
func TestEveryStepDescribesItself(t *testing.T) {
	for i, s := range DefaultRecipe(testVersion).Steps() {
		if strings.TrimSpace(s.Desc) == "" {
			t.Errorf("step %d (%s) has no description", i, s.Kind)
		}
		switch s.Kind {
		case StepRun:
			if len(s.Argv) == 0 {
				t.Errorf("step %d is a run step with no command", i)
			}
		case StepWriteFile, StepAppendFile:
			if s.Path == "" {
				t.Errorf("step %d is a %s step with no path", i, s.Kind)
			}
		default:
			t.Errorf("step %d has unknown kind %q", i, s.Kind)
		}
	}
}
