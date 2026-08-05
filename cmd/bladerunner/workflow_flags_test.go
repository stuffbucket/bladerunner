package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// The release workflow drives `br` as a subprocess, so the compiler cannot see
// that its command line matches this binary's flags. A flag removed here goes on
// compiling, passes every test, merges, and then fails the first real guest image
// build with "unknown flag".
//
// That is not hypothetical. `--method` was deleted along with the two build
// mechanics that were never implemented, and the workflow still passed it. Both
// architectures failed on the merge commit. Nothing could have caught it: this
// workflow only runs on pushes to main, so the pull request that broke it never
// ran the job it broke.
//
// So the check has to live where it runs on every change — here. It DERIVES the
// flags from the workflow rather than listing them, so a flag added to the
// workflow tomorrow is checked without editing this file.
func TestWorkflowsOnlyPassFlagsTheCLIDefines(t *testing.T) {
	root := repoRootForWorkflows(t)
	workflows := filepath.Join(root, ".github", "workflows")

	entries, err := os.ReadDir(workflows)
	if err != nil {
		t.Fatalf("read %s: %v", workflows, err)
	}

	checked := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		path := filepath.Join(workflows, e.Name())
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, inv := range brInvocations(string(body)) {
			checked++
			cmd, args := resolveCommand(t, inv.words)
			for _, flag := range inv.flags {
				if cmd.Flags().Lookup(flag) == nil && cmd.PersistentFlags().Lookup(flag) == nil {
					t.Errorf("%s runs `br %s --%s`, but %q defines no such flag",
						e.Name(), strings.Join(inv.words, " "), flag, strings.Join(args, " "))
				}
			}
		}
	}

	// A parser that silently matches nothing would pass this test forever while
	// checking nothing at all.
	if checked == 0 {
		t.Fatal("found no br invocations in any workflow; the parser has stopped matching")
	}
}

// invocation is one `br ...` command line found in a workflow.
type invocation struct {
	// words are the subcommand path, e.g. ["disk", "bake"].
	words []string
	// flags are the long flag names passed, without the leading dashes.
	flags []string
}

// brInvocationLine matches the start of a br command line, however it is
// spelled: `./bin/br`, `bin/br`, or bare `br`, with or without sudo.
var brInvocationLine = regexp.MustCompile(`^\s*(?:sudo\s+)?\.?/?(?:bin/)?br\s+(.*)$`)

// flagToken matches a long flag anywhere in a command line.
var flagToken = regexp.MustCompile(`--([a-z][a-z0-9-]*)`)

// brInvocations finds every br command line in a workflow, joining the
// backslash continuations a multi-line invocation is written with.
//
// Line-based rather than one regex over the whole body. The first version
// scanned the body and tracked an offset, and got the continuation join wrong in
// a way that silently dropped every line after the first — so it checked only
// `--arch` and passed while the flag that broke the build sat one line below.
// The bug survived because the test still passed; it took mutating the workflow
// and watching it NOT fail to find it.
func brInvocations(body string) []invocation {
	var out []invocation

	lines := strings.Split(body, "\n")
	for i := 0; i < len(lines); i++ {
		m := brInvocationLine.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}

		line := m[1]
		for strings.HasSuffix(strings.TrimRight(line, " "), `\`) && i+1 < len(lines) {
			i++
			line = strings.TrimSuffix(strings.TrimRight(line, " "), `\`) + " " + strings.TrimSpace(lines[i])
		}

		inv := invocation{}
		for _, w := range strings.Fields(line) {
			if strings.HasPrefix(w, "-") {
				break // The subcommand path ends at the first flag.
			}
			inv.words = append(inv.words, w)
		}
		for _, f := range flagToken.FindAllStringSubmatch(line, -1) {
			inv.flags = append(inv.flags, f[1])
		}
		if len(inv.words) > 0 {
			out = append(out, inv)
		}
	}
	return out
}

// resolveCommand walks the real command tree to the deepest subcommand the
// words name, and returns it with the path actually resolved. Trailing words
// that are positional arguments rather than subcommands simply stop the walk.
func resolveCommand(t *testing.T, words []string) (*cobra.Command, []string) {
	t.Helper()

	cmd := rootCmd
	resolved := []string{"br"}
	for _, w := range words {
		next, _, err := cmd.Find([]string{w})
		if err != nil || next == cmd {
			break
		}
		cmd = next
		resolved = append(resolved, w)
	}
	return cmd, resolved
}

// repoRootForWorkflows walks up until it finds the directory holding go.mod.
func repoRootForWorkflows(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod found above the working directory")
		}
		dir = parent
	}
}
