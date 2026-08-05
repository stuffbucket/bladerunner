package main

import (
	"bytes"
	"strings"
	"testing"
)

// `br version` must write to STDOUT.
//
// Cobra's cmd.Println writes to OutOrStderr, so the obvious implementation puts
// the version on stderr and `br version | cat` prints nothing. A version string
// is the most likely output in this CLI to be piped into something.
func TestVersionGoesToStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	versionCmd.SetOut(&stdout)
	versionCmd.SetErr(&stderr)
	t.Cleanup(func() { versionCmd.SetOut(nil); versionCmd.SetErr(nil) })

	if err := versionCmd.RunE(versionCmd, nil); err != nil {
		t.Fatalf("br version: %v", err)
	}

	if got := strings.TrimSpace(stdout.String()); got == "" {
		t.Error("br version wrote nothing to stdout")
	}
	if got := stderr.String(); got != "" {
		t.Errorf("br version wrote %q to stderr; it belongs on stdout", got)
	}
}

// The subcommand and the flag must not drift into two spellings of one fact.
//
// They are separate cobra mechanisms — RunE versus SetVersionTemplate — so
// nothing but this test stops one being updated and the other left behind.
func TestVersionSubcommandMatchesTheFlag(t *testing.T) {
	var stdout bytes.Buffer
	versionCmd.SetOut(&stdout)
	t.Cleanup(func() { versionCmd.SetOut(nil) })

	if err := versionCmd.RunE(versionCmd, nil); err != nil {
		t.Fatalf("br version: %v", err)
	}

	sub := strings.TrimSpace(stdout.String())
	flag := strings.TrimSpace(rootCmd.VersionTemplate())

	if !strings.Contains(flag, sub) {
		t.Errorf("`br version` prints %q but `br --version` renders %q", sub, flag)
	}
}

// The version must actually name the binary, so a pasted line is identifiable
// without the command that produced it.
func TestVersionLineNamesTheBinary(t *testing.T) {
	if got := versionLine(); !strings.HasPrefix(got, "br version ") {
		t.Errorf("versionLine() = %q, want it to start with %q", got, "br version ")
	}
}
