package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/imagebuild"
)

// `br disk bake` advertised --debian-release, and nothing read it. The build is
// hardwired to Trixie: the image filename, the optional Incus UI suite, and the
// recipe's package and service assumptions are all Debian 13's. So
// `--debian-release bookworm` produced a Trixie image and said nothing (#243).
//
// A flag that is ignored is worse than a flag that is missing. A missing one
// fails at the shell with "unknown flag"; an ignored one succeeds, and the
// operator believes they built something they did not.

// debianReleaseFlagName is the removed flag. It is named here rather than
// inlined so the test says what it is looking for.
const debianReleaseFlagName = "debian-release"

// No command may advertise a release override, because no command implements
// one. The walk covers the whole tree rather than `disk bake` alone: the same
// flag was also advertised by the shell builder this command replaced, and a
// reintroduction anywhere is the same defect.
func TestNoCommandAdvertisesADebianReleaseOverride(t *testing.T) {
	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		if cmd.Flags().Lookup(debianReleaseFlagName) != nil {
			t.Errorf("'%s' advertises --%s, but the release comes from the reviewed pins in internal/imagebuild; a value passed here is silently ignored",
				cmd.CommandPath(), debianReleaseFlagName)
		}
		for _, sub := range cmd.Commands() {
			walk(sub)
		}
	}
	walk(rootCmd)
}

// Removing the flag only helps if the help then says what the builder does
// build. The suite is read from internal/imagebuild rather than written here,
// so a deliberate move to a newer Debian fails this test until the help text
// moves with it — which is the whole point of taking the choice away from the
// command line.
func TestDiskBakeHelpNamesTheOnlySuiteItBuilds(t *testing.T) {
	suites := make(map[string]bool)
	for _, arch := range scaffoldArchList {
		release, err := imagebuild.BaseRelease(arch)
		if err != nil {
			t.Fatalf("BaseRelease(%q): %v", arch, err)
		}
		suites[release.Suite] = true
	}
	if len(suites) != 1 {
		t.Fatalf("the builder resolves to %d suites %v; it can only claim to own one", len(suites), suites)
	}

	help := strings.ToLower(diskBakeCmd.Long)
	for suite := range suites {
		if !strings.Contains(help, suite) {
			t.Errorf("'br disk bake' help does not name %q, the only Debian suite it can build", suite)
		}
	}
}
