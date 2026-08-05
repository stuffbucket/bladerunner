package main

import (
	"slices"
	"testing"
)

// aliasCase is one verb another tool uses and the bladerunner command it must
// reach.
type aliasCase struct {
	typed string
	want  string
	from  string
}

// Verbs from other tools must RESOLVE, not merely be suggested.
//
// A suggestion still costs the user a second attempt. Where the meaning is the
// same and only the spelling differs, the word should just work.
var aliasCases = []aliasCase{
	{typed: "delete", want: "reset", from: "colima delete"},
	{typed: "list", want: "instances", from: "colima list"},
	{typed: "ps", want: "ls", from: "docker ps"},
	{typed: "template", want: "config", from: "colima template"},
	{typed: "update", want: "self-update", from: "colima update"},
}

func TestOtherToolsVerbsResolve(t *testing.T) {
	for _, tc := range aliasCases {
		t.Run(tc.typed, func(t *testing.T) {
			cmd, _, err := rootCmd.Find([]string{tc.typed})
			if err != nil {
				t.Fatalf("`br %s` (%s) does not resolve: %v", tc.typed, tc.from, err)
			}
			if cmd.Name() != tc.want {
				t.Errorf("`br %s` (%s) resolves to %q, want %q", tc.typed, tc.from, cmd.Name(), tc.want)
			}
		})
	}
}

// ls and list are DIFFERENT commands, on purpose.
//
// `br ls` lists Incus instances inside the guest; `br list` lists the VMs
// themselves. The names are close and the meanings are not. That is a
// deliberate call: a colima user typing `list` means VMs, a docker user typing
// `ps` means containers, and both now get what they meant. This pins that they
// stay distinct rather than one quietly becoming an alias of the other.
func TestLsAndListStayDistinct(t *testing.T) {
	ls, _, err := rootCmd.Find([]string{"ls"})
	if err != nil {
		t.Fatalf("br ls does not resolve: %v", err)
	}
	list, _, err := rootCmd.Find([]string{"list"})
	if err != nil {
		t.Fatalf("br list does not resolve: %v", err)
	}
	if ls.Name() == list.Name() {
		t.Errorf("br ls and br list both resolve to %q; they list different things", ls.Name())
	}
	if ls.Name() != "ls" || list.Name() != "instances" {
		t.Errorf("ls -> %q, list -> %q; want ls -> ls, list -> instances", ls.Name(), list.Name())
	}
}

// Synonyms with no exact counterpart stay SUGGESTIONS.
//
// These mean something near a bladerunner verb without meaning the same thing,
// so pointing is honest where aliasing would over-promise. `br destroy` should
// mention reset, not silently perform it.
func TestApproximateSynonymsAreOnlySuggested(t *testing.T) {
	for _, tc := range []aliasCase{
		{typed: "destroy", want: "reset", from: "vagrant destroy"},
		{typed: "rm", want: "reset", from: "docker rm"},
		{typed: "halt", want: "stop", from: "vagrant halt"},
		{typed: "down", want: "stop", from: "docker compose down"},
		{typed: "console", want: "shell", from: "serial console"},
	} {
		t.Run(tc.typed, func(t *testing.T) {
			if cmd, _, err := rootCmd.Find([]string{tc.typed}); err == nil && cmd.Name() == tc.typed {
				t.Fatalf("%q became a real command; it was meant to stay a suggestion", tc.typed)
			}
			if got := rootCmd.SuggestionsFor(tc.typed); !slices.Contains(got, tc.want) {
				t.Errorf("`br %s` (%s) suggests %v, want %q among them", tc.typed, tc.from, got, tc.want)
			}
		})
	}
}
