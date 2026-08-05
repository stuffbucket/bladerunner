package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// The documentation is a promise about a different component: the binary this
// package builds. Issue #226 found the promise broken in five files — commands
// spelled `runner` (the pre-#84 name), `make all` (never a target), and `br
// start --network-mode/--bridge-interface/--log-path` (flags that moved into
// the persisted settings document). Every one of them fails on paste, and
// RELEASE.md's were in the release verification step, so a valid release could
// not be signed off by following its own checklist.
//
// AGENTS.md section 5.7: a claim about another component needs a test that
// holds it. These read the shell blocks out of the docs and resolve every
// command against the real Cobra tree and the real Makefile.

// docRoot is the repository root as seen from this package's directory.
const docRoot = "../.."

// docShellLangs are the fenced-code-block languages that hold commands a reader
// is expected to run. A ```text or ```json block is a listing, not a command.
var docShellLangs = map[string]bool{"bash": true, "sh": true, "shell": true, "console": true}

// docFiles returns the user-facing prose whose commands must resolve.
//
// docs/instance-floppies/ is excluded on purpose: it is an unimplemented design
// proposal (`br floppy insert …`), so its commands describe a binary that does
// not exist yet. CHANGELOG.md is excluded because release-please generates it
// from historical commit subjects, which correctly record the old name.
func docFiles(t *testing.T) []string {
	t.Helper()
	files := []string{
		filepath.Join(docRoot, "README.md"),
		filepath.Join(docRoot, "CONTRIBUTING.md"),
		filepath.Join(docRoot, "RELEASE.md"),
	}
	err := filepath.WalkDir(filepath.Join(docRoot, "docs"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "instance-floppies" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".md") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk docs: %v", err)
	}
	return files
}

// docCommand is one runnable line lifted out of a doc's shell block.
type docCommand struct {
	file string
	line int
	text string
}

// shellCommands reads a markdown file and returns the lines inside its shell
// code blocks.
func shellCommands(t *testing.T, path string) []docCommand {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var (
		out    []docCommand
		inside bool
		lang   string
	)
	for i, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "```") {
			if inside {
				inside = false
				continue
			}
			inside, lang = true, strings.TrimSpace(strings.TrimPrefix(line, "```"))
			continue
		}
		if inside && docShellLangs[lang] && line != "" {
			out = append(out, docCommand{file: path, line: i + 1, text: line})
		}
	}
	return out
}

// commandTokens strips a shell line down to the words that name a command: the
// leading `$` prompt and any VAR=value prefix go, and a trailing `# comment`
// goes with them.
func commandTokens(line string) []string {
	if idx := strings.Index(line, " #"); idx >= 0 {
		line = line[:idx]
	}
	fields := strings.Fields(strings.TrimPrefix(line, "$ "))
	for len(fields) > 0 && !strings.HasPrefix(fields[0], "-") && strings.Contains(fields[0], "=") {
		fields = fields[1:]
	}
	return fields
}

// TestDocsDoNotNameTheOldBinary rejects the pre-#84 `runner` spelling anywhere a
// reader would paste it. Prose ("a standalone Incus VM runner", "a self-hosted
// macOS runner") is untouched: only shell blocks are read.
func TestDocsDoNotNameTheOldBinary(t *testing.T) {
	for _, file := range docFiles(t) {
		for _, cmd := range shellCommands(t, file) {
			if staleBinaryName.MatchString(cmd.text) {
				t.Errorf("%s:%d names the removed binary \"runner\"; it is \"br\":\n  %s",
					cmd.file, cmd.line, cmd.text)
			}
		}
	}
}

// makeTargets reads the phony and real target names out of the Makefile.
func makeTargets(t *testing.T) map[string]bool {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(docRoot, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	targets := make(map[string]bool)
	for _, raw := range strings.Split(string(body), "\n") {
		if raw == "" || raw[0] == '\t' || raw[0] == '#' || raw[0] == ' ' {
			continue
		}
		name, _, found := strings.Cut(raw, ":")
		if !found || strings.Contains(name, "=") || strings.Contains(name, " ") {
			continue
		}
		if name == ".PHONY" {
			continue
		}
		targets[name] = true
	}
	if !targets["check"] {
		t.Fatal("Makefile parse found no 'check' target; the parser is wrong, not the docs")
	}
	return targets
}

// TestDocumentedMakeTargetsExist catches a documented `make <target>` that the
// Makefile does not define. `make all` survived in CONTRIBUTING.md's quick start
// with no such target ever existing.
func TestDocumentedMakeTargetsExist(t *testing.T) {
	targets := makeTargets(t)
	for _, file := range docFiles(t) {
		for _, cmd := range shellCommands(t, file) {
			tokens := commandTokens(cmd.text)
			if len(tokens) < 2 || tokens[0] != "make" {
				continue
			}
			if !targets[tokens[1]] {
				t.Errorf("%s:%d runs 'make %s', which the Makefile does not define:\n  %s",
					cmd.file, cmd.line, tokens[1], cmd.text)
			}
		}
	}
}

// resolveDocSubcommand walks the Cobra tree down the leading words of a
// documented command line and returns the command it lands on, plus the tokens
// left over.
func resolveDocSubcommand(tokens []string) (*cobra.Command, []string) {
	cmd := rootCmd
	rest := tokens[1:]
	for len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		sub := findDocSubcommand(cmd, rest[0])
		if sub == nil {
			break
		}
		cmd, rest = sub, rest[1:]
	}
	// Cobra adds --help and --version only when the command executes, so a doc
	// line using either would otherwise look like it passes an unknown flag.
	cmd.InitDefaultHelpFlag()
	cmd.InitDefaultVersionFlag()
	return cmd, rest
}

// findDocSubcommand returns the child command a word names, by name or alias.
func findDocSubcommand(cmd *cobra.Command, word string) *cobra.Command {
	for _, sub := range cmd.Commands() {
		if sub.Name() == word {
			return sub
		}
		for _, alias := range sub.Aliases {
			if alias == word {
				return sub
			}
		}
	}
	return nil
}

// docFlagKnown reports whether a command accepts the flag token, counting the
// persistent flags it inherits from its parents.
func docFlagKnown(cmd *cobra.Command, token string) bool {
	if long, ok := strings.CutPrefix(token, "--"); ok {
		name, _, _ := strings.Cut(long, "=")
		return cmd.Flags().Lookup(name) != nil || cmd.InheritedFlags().Lookup(name) != nil
	}
	short := strings.TrimPrefix(token, "-")
	name, _, _ := strings.Cut(short, "=")
	if name == "" {
		return true // a bare "-" is a stdin placeholder, not a flag
	}
	return cmd.Flags().ShorthandLookup(name[:1]) != nil ||
		cmd.InheritedFlags().ShorthandLookup(name[:1]) != nil
}

// TestDocumentedCommandsResolve holds every documented `br …` invocation against
// the live command tree: the verb must exist and every flag must be registered
// on it. This is the check that would have caught `br start --network-mode`,
// `--bridge-interface` and `--log-path` still being documented after they moved
// into the persisted settings document.
func TestDocumentedCommandsResolve(t *testing.T) {
	for _, file := range docFiles(t) {
		for _, doc := range shellCommands(t, file) {
			tokens := commandTokens(doc.text)
			if len(tokens) == 0 || tokens[0] != rootCmd.Name() {
				continue
			}
			cmd, rest := resolveDocSubcommand(tokens)
			for _, token := range rest {
				if token == "--" {
					break // everything after it belongs to the guest
				}
				if !strings.HasPrefix(token, "-") {
					continue // a positional argument or a placeholder
				}
				if !docFlagKnown(cmd, token) {
					t.Errorf("%s:%d passes %s to '%s', which does not accept it:\n  %s",
						doc.file, doc.line, token, cmd.CommandPath(), doc.text)
				}
			}
		}
	}
}

// TestDocScannerReadsTheDocs guards the three tests above against silently
// scanning nothing — a parser that matched no file would pass forever.
func TestDocScannerReadsTheDocs(t *testing.T) {
	files := docFiles(t)
	if len(files) < 4 {
		t.Fatalf("docFiles found %d files, want the three root docs plus docs/", len(files))
	}
	var brLines int
	for _, file := range files {
		for _, doc := range shellCommands(t, file) {
			if tokens := commandTokens(doc.text); len(tokens) > 0 && tokens[0] == rootCmd.Name() {
				brLines++
			}
		}
	}
	if brLines == 0 {
		t.Fatal("no documented 'br' command lines were found; the scanner is broken")
	}
}
