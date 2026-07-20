package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestRejectJSONForInteractive verifies the shared guard used by the streaming
// verbs: it errors (and names the command) when --json is set, and is a no-op
// otherwise. The error message is the contract agents parse, so pin it.
func TestRejectJSONForInteractive(t *testing.T) {
	tests := []struct {
		name    string
		cmdName string
		json    bool
		wantErr bool
	}{
		{"json off is a no-op", "shell", false, false},
		{"json on rejects shell", "shell", true, true},
		{"json on rejects exec", "exec", true, true},
		{"json on rejects incus", "incus", true, true},
		{"json on rejects events", "events", true, true},
		{"json on rejects logs", "logs", true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prev := jsonOutput
			jsonOutput = tc.json
			defer func() { jsonOutput = prev }()

			err := rejectJSONForInteractive(tc.cmdName)
			if tc.wantErr != (err != nil) {
				t.Fatalf("rejectJSONForInteractive(%q) with json=%v: err=%v, wantErr=%v",
					tc.cmdName, tc.json, err, tc.wantErr)
			}
			if err == nil {
				return
			}
			if !strings.Contains(err.Error(), tc.cmdName) {
				t.Errorf("error %q does not name the command %q", err, tc.cmdName)
			}
			if !strings.Contains(err.Error(), "--json is not supported") {
				t.Errorf("error %q missing the stable guard phrase", err)
			}
		})
	}
}

// TestRejectJSONForInteractiveWiredEverywhere guards against a future verb
// forgetting the shared guard: every interactive/streaming verb must return the
// guard error when --json is set.
func TestRejectJSONForInteractiveWiredEverywhere(t *testing.T) {
	prev := jsonOutput
	jsonOutput = true
	defer func() { jsonOutput = prev }()

	// These are the verbs the guard covers (issue #135). runE is invoked with
	// args that would otherwise reach real work; the guard must short-circuit
	// before that, so a nil/guard error is the only acceptable outcome.
	cmds := map[string]*cobra.Command{
		"shell":  shellCmd,
		"exec":   execCmd,
		"incus":  incusCmd,
		"events": eventsCmd,
		"logs":   logsCmd,
	}
	for name, c := range cmds {
		t.Run(name, func(t *testing.T) {
			if c.RunE == nil {
				t.Fatalf("%s has no RunE", name)
			}
			err := c.RunE(c, nil)
			if err == nil {
				t.Fatalf("%s did not reject --json", name)
			}
			if !strings.Contains(err.Error(), "--json is not supported") {
				t.Fatalf("%s returned %q, not the --json guard", name, err)
			}
		})
	}
}

// TestUpCmdWiring verifies br up is registered under the Lifecycle group and
// wired to the shared start path (no duplicate RunE literal).
func TestUpCmdWiring(t *testing.T) {
	if upCmd.Use != "up" {
		t.Fatalf("upCmd.Use = %q, want %q", upCmd.Use, "up")
	}
	if upCmd.GroupID != groupLifecycle {
		t.Errorf("upCmd.GroupID = %q, want %q", upCmd.GroupID, groupLifecycle)
	}
	if upCmd.RunE == nil {
		t.Fatal("upCmd.RunE is nil")
	}
	if err := upCmd.Args(upCmd, []string{"unexpected"}); err == nil {
		t.Error("up should reject positional args (use 'br start' for flags)")
	}

	// up must be reachable from the root command.
	var found bool
	for _, c := range rootCmd.Commands() {
		if c == upCmd {
			found = true
			break
		}
	}
	if !found {
		t.Error("upCmd is not registered on rootCmd")
	}
}

// TestCommandGroupsAssigned enforces issue #131's "assign a GroupID to ALL
// verbs" note: a missing GroupID makes cobra render a stray "Additional
// Commands" bucket. The built-in help/completion commands are exempted (they're
// pinned via SetHelpCommandGroupID/SetCompletionCommandGroupID).
func TestCommandGroupsAssigned(t *testing.T) {
	valid := map[string]bool{
		groupLifecycle: true,
		groupAccess:    true,
		groupMedia:     true,
		groupUI:        true,
		groupConfig:    true,
	}
	for _, c := range rootCmd.Commands() {
		if c.Name() == "help" || c.Name() == "completion" {
			continue
		}
		if !valid[c.GroupID] {
			t.Errorf("command %q has GroupID %q, not one of the defined groups", c.Name(), c.GroupID)
		}
	}
}

// TestRootGroupsRegistered verifies each GroupID a command references is backed
// by a registered cobra.Group — an unregistered GroupID panics at Execute time.
func TestRootGroupsRegistered(t *testing.T) {
	registered := map[string]bool{}
	for _, g := range rootCmd.Groups() {
		registered[g.ID] = true
	}
	for _, id := range []string{groupLifecycle, groupAccess, groupMedia, groupUI, groupConfig} {
		if !registered[id] {
			t.Errorf("group %q is referenced but not registered via AddGroup", id)
		}
	}
}
