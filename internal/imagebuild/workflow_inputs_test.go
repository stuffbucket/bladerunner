package imagebuild

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// workflowPath locates the guest-image release workflow. Like the build script
// it is on its way out, so an absent file skips rather than fails.
func workflowPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", ".github", "workflows", "build-guest-image.yml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("release workflow not present at %s", path)
	}
	return path
}

// scriptRepoPath matches a repository file the build script reads through its
// own directory, e.g. ${SCRIPT_DIR}/../internal/imagebuild/basepins.sha512.
var scriptRepoPath = regexp.MustCompile(`\$\{SCRIPT_DIR\}/\.\./([A-Za-z0-9_./-]+)`)

// A push workflow rebuilds only when a file it lists under `paths` changes. So
// every repository file the build reads is part of the build's input set, and
// one that is missing from that list produces a silent staleness: the input
// changes, no rebuild runs, and the published image keeps being made from the
// old value with nothing in either place to notice.
//
// This bit immediately. Pinning the base image made the script read
// internal/imagebuild/basepins.sha512, which means bumping the Debian pin — the
// single act that most needs to produce a new image — would not have rebuilt
// anything.
//
// AGENTS.md rule 5.7: the claim "the workflow rebuilds when its inputs change"
// spans two files that never reference each other, so it is held here rather
// than asserted in a comment.
func TestWorkflowRebuildsOnEveryScriptInput(t *testing.T) {
	script, err := os.ReadFile(buildScriptPath(t))
	if err != nil {
		t.Fatalf("read build script: %v", err)
	}
	workflow, err := os.ReadFile(workflowPath(t))
	if err != nil {
		t.Fatalf("read release workflow: %v", err)
	}
	triggers := triggerPaths(string(workflow))
	if len(triggers) == 0 {
		t.Fatal("the release workflow lists no push paths; every change would rebuild, or none would")
	}

	for _, match := range scriptRepoPath.FindAllStringSubmatch(string(script), -1) {
		input := match[1]
		if !coveredByTrigger(input, triggers) {
			t.Errorf("the build script reads %s, but no push path in the workflow covers it;\n"+
				"changing it would not rebuild the published image\n  trigger paths: %v",
				input, triggers)
		}
	}
}

// triggerPaths pulls the `paths:` list out of the workflow's push trigger.
//
// It reads the YAML as lines rather than parsing it, because the repository
// takes no new module dependencies (AGENTS.md rule 5.1) and the shape here is a
// flat list of literals.
//
// Comments and blank lines inside the list are skipped rather than treated as
// its end. That is not defensive coding: the list is annotated, and an earlier
// version of this function ended the list at the first annotation, silently
// dropping every entry below it and reporting a gap that did not exist.
func triggerPaths(workflow string) []string {
	var out []string
	inPaths := false
	for _, line := range strings.Split(workflow, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "paths:":
			inPaths = true
		case !inPaths:
			// Still above the list.
		case trimmed == "" || strings.HasPrefix(trimmed, "#"):
			// An annotation inside the list.
		case strings.HasPrefix(trimmed, "- "):
			out = append(out, strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")))
		default:
			// The next key ends the list.
			return out
		}
	}
	return out
}

// coveredByTrigger reports whether a repository path would match one of the
// workflow's push paths. Only exact equality and the trailing `**` form are
// recognized, which are the two the workflow uses; anything more would be
// guessing at GitHub's matcher rather than reading it.
//
// A `dir/**` trigger covers the directory itself as well as everything under
// it, because the script names a directory without a trailing slash when it
// cd's into one.
func coveredByTrigger(input string, triggers []string) bool {
	for _, trigger := range triggers {
		if trigger == input {
			return true
		}
		prefix, isTree := strings.CutSuffix(trigger, "**")
		if !isTree {
			continue
		}
		dir := strings.TrimSuffix(prefix, "/")
		if input == dir || strings.HasPrefix(input, dir+"/") {
			return true
		}
	}
	return false
}
