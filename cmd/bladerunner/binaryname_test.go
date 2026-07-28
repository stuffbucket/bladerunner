package main

import (
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// staleBinaryName matches the binary's old name as a standalone word, so
// "bladerunner" (the module, the app bundle, the prose name) does not match.
var staleBinaryName = regexp.MustCompile(`(^|[^0-9A-Za-z])runner\b`)

// The binary is `br` (see rootCmd.Use). Several help strings still called it
// `runner`, a command that does not exist — including two ERROR strings a
// mistyping user is told to act on ("usage: runner config get <key>").
func TestHelpTextNamesTheBinary(t *testing.T) {
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		for _, text := range []struct{ what, s string }{
			{"Short", cmd.Short},
			{"Long", cmd.Long},
			{"Example", cmd.Example},
		} {
			if staleBinaryName.MatchString(text.s) {
				t.Errorf("'%s' %s calls the binary \"runner\"; it is \"br\":\n%s",
					cmd.CommandPath(), text.what, text.s)
			}
		}
		// FlagUsages rather than VisitAll: pflag is an indirect module
		// dependency and importing it here would change go.mod (see
		// rootPersistentFlagNames).
		if usages := cmd.Flags().FlagUsages(); staleBinaryName.MatchString(usages) {
			t.Errorf("a flag of '%s' calls the binary \"runner\":\n%s", cmd.CommandPath(), usages)
		}
		for _, sub := range cmd.Commands() {
			walk(sub)
		}
	}
	walk(rootCmd)
}

// The two usage errors are what a user who mistyped is told to run next, so
// they have to name a command that exists.
func TestConfigUsageErrorsNameTheBinary(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"get with no key", runConfigGet(nil), "br config get"},
		{"set with no value", runConfigSet([]string{"cpus"}), "br config set"},
	} {
		if tc.err == nil {
			t.Errorf("%s: no error", tc.name)
			continue
		}
		if !strings.Contains(tc.err.Error(), tc.want) {
			t.Errorf("%s: error %q does not say %q", tc.name, tc.err, tc.want)
		}
		if staleBinaryName.MatchString(tc.err.Error()) {
			t.Errorf("%s: error %q still calls the binary \"runner\"", tc.name, tc.err)
		}
	}
}
