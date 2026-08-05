package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The Makefile must stamp the binary from a PRODUCT tag.
//
// `git describe --tags` considers every tag, and this repo also publishes
// guest-image-vYYYY.MM.DD releases. Those are routinely more recent than the
// last product tag, so an unrestricted describe stamps the binary with an image
// build date and `br --version` reports a qcow2 release (#294).
//
// This is asserted against the Makefile text rather than by running `make`,
// because the failure is a missing flag rather than a wrong output: on a
// checkout whose most recent tag happens to be a product tag, the broken recipe
// and the correct one produce identical output, so a behavioral test would pass
// while the defect sat there waiting for the next guest-image publish.
func TestMakefileStampsFromAProductTag(t *testing.T) {
	const makefile = "../../Makefile"

	data, err := os.ReadFile(makefile)
	if err != nil {
		t.Fatalf("read %s: %v", makefile, err)
	}

	line := versionRecipe(string(data))
	if line == "" {
		t.Fatalf("no VERSION assignment found in %s", makefile)
	}
	if !strings.Contains(line, "git describe") {
		return // not derived from git; nothing for this rule to constrain
	}
	if !strings.Contains(line, "--match") {
		t.Errorf("VERSION runs `git describe` with no --match, so a guest-image tag can stamp the binary:\n  %s", line)
	}
}

// versionRecipe returns the Makefile's VERSION assignment, or "" if absent.
func versionRecipe(makefile string) string {
	re := regexp.MustCompile(`(?m)^VERSION\s*[:?]?=.*$`)
	return re.FindString(makefile)
}
