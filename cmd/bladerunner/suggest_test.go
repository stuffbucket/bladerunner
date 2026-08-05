package main

import (
	"slices"
	"testing"
)

// A user arriving from another tool types the verb that tool used. Cobra
// already suggests NEAR-MISSES by edit distance, so "resart" finds restart —
// but a synonym is not a near-miss. `br delete` is nowhere near `br reset`, and
// without a declared synonym it gets "unknown command" and nothing else.
//
// These are suggestions, not aliases. Adding `br delete` as a real verb, or a
// third spelling of the two list commands, would trade one confusion for
// another; pointing at the right verb and letting the user choose does not.
func TestSynonymsFromOtherToolsAreSuggested(t *testing.T) {
	cases := []struct {
		typed string
		want  string
		from  string
	}{
		{typed: "delete", want: "reset", from: "colima delete"},
		{typed: "destroy", want: "reset", from: "vagrant destroy"},
		{typed: "rm", want: "reset", from: "docker rm"},
		{typed: "teardown", want: "reset", from: "colima's own wording"},
		{typed: "list", want: "instances", from: "colima list"},
		{typed: "halt", want: "stop", from: "vagrant halt"},
		{typed: "down", want: "stop", from: "docker compose down"},
		{typed: "console", want: "shell", from: "multipass/serial console"},
	}

	for _, tc := range cases {
		t.Run(tc.typed, func(t *testing.T) {
			got := rootCmd.SuggestionsFor(tc.typed)
			if !slices.Contains(got, tc.want) {
				t.Errorf("`br %s` (%s) suggests %v, want %q among them",
					tc.typed, tc.from, got, tc.want)
			}
		})
	}
}

// A synonym must not become a real command.
//
// The suggestion exists precisely so the verb does NOT have to be added: two
// commands that both list things under confusingly similar names is the state
// this avoids.
func TestSuggestedSynonymsAreNotRealCommands(t *testing.T) {
	for _, name := range []string{"delete", "destroy", "rm", "list", "halt", "down"} {
		cmd, _, err := rootCmd.Find([]string{name})
		if err == nil && cmd.Name() == name {
			t.Errorf("%q became a real command; it was meant to stay a suggestion", name)
		}
	}
}
