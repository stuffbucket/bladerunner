package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// collectInstancePolicies walks the whole command tree and records the --instance
// policy each command resolves to.
func collectInstancePolicies(cmd *cobra.Command, into map[string]string) {
	for _, child := range cmd.Commands() {
		into[child.CommandPath()] = instancePolicy(child)
		collectInstancePolicies(child, into)
	}
}

// Cobra renders --instance in every command's help, so every command owes the
// user an answer about it: honor it, deliberately span every instance, or
// refuse it. Undeclared means refused, so a new verb cannot quietly accept the
// flag and ignore it — but it can still be added without anyone THINKING about
// the flag, which is what this table is for. A new command shows up here as a
// failure and forces the decision into the diff (issue #9).
func TestEveryCommandDeclaresAnInstancePolicy(t *testing.T) {
	want := map[string]string{
		// Acts on the instance the user selected.
		"br config":      instanceHonored,
		"br eject":       instanceHonored,
		"br events":      instanceHonored,
		"br exec":        instanceHonored,
		"br incus":       instanceHonored,
		"br logs":        instanceHonored,
		"br ls":          instanceHonored,
		"br reconnect":   instanceHonored,
		"br reset":       instanceHonored,
		"br restore":     instanceHonored,
		"br save":        instanceHonored,
		"br shell":       instanceHonored,
		"br ssh":         instanceHonored,
		"br status":      instanceHonored,
		"br restart":     instanceHonored,
		"br stop":        instanceHonored,
		"br upgrade":     instanceHonored,
		"br web":         instanceHonored,
		"br web approve": instanceHonored,
		"br web trust":   instanceHonored,

		// Spans every instance on purpose.
		"br instances": instanceAllInstances,

		// Creates or selects an instance rather than acting on one, or does not
		// touch a VM at all.
		"br boot":                  instanceRefused,
		"br completion":            instanceRefused,
		"br version":               instanceRefused,
		"br completion bash":       instanceRefused,
		"br completion fish":       instanceRefused,
		"br completion powershell": instanceRefused,
		"br completion zsh":        instanceRefused,
		"br disk":                  instanceRefused,
		"br disk bake":             instanceRefused,
		"br disk new":              instanceRefused,
		"br disk pack":             instanceRefused,
		"br disks":                 instanceRefused,
		"br help":                  instanceRefused,
		"br menubar":               instanceRefused,
		"br menubar bundle":        instanceRefused,
		"br menubar install":       instanceRefused,
		"br menubar uninstall":     instanceRefused,
		"br notice":                instanceRefused,
		"br self-update":           instanceRefused,
		"br start":                 instanceRefused,
		"br up":                    instanceRefused,
		"br user":                  instanceRefused,
		"br user add":              instanceRefused,
		"br user list":             instanceRefused,
		"br user remove":           instanceRefused,
		"br vmd":                   instanceRefused,
		"br watch":                 instanceRefused,
		"br web untrust":           instanceRefused,
	}

	got := map[string]string{}
	// Cobra adds `help` and `completion` lazily, on Execute. Force them in so
	// the walk sees the tree a user actually gets.
	rootCmd.InitDefaultHelpCmd()
	rootCmd.InitDefaultCompletionCmd()
	collectInstancePolicies(rootCmd, got)

	for path, policy := range got {
		expected, listed := want[path]
		if !listed {
			t.Errorf("command %q has no declared --instance policy in this test; decide whether it honors, spans or refuses the flag", path)
			continue
		}
		if policy != expected {
			t.Errorf("command %q resolves --instance policy %q, want %q", path, policy, expected)
		}
	}
	for path := range want {
		if _, present := got[path]; !present {
			t.Errorf("this test expects a command %q that the tree no longer has", path)
		}
	}
}

// A command that does not act on a selected instance must say so, not accept
// the flag and ignore it. The refusal names the command and, where there is a
// better verb, names that too.
func TestCheckInstanceFlagRefusesWhereItIsMeaningless(t *testing.T) {
	cases := []struct {
		name    string
		cmd     *cobra.Command
		flag    string
		wantErr []string
	}{
		{name: "no flag, no refusal", cmd: upCmd, flag: ""},
		{name: "honored command", cmd: statusCmd, flag: "demo"},
		{name: "honored subcommand inherits", cmd: webTrustCmd, flag: "demo"},
		{name: "listing spans every instance", cmd: instancesCmd, flag: "demo"},
		{
			name: "up creates an instance", cmd: upCmd, flag: "demo",
			wantErr: []string{"br up", "--instance", "br boot"},
		},
		{
			name: "start creates an instance", cmd: startCmd, flag: "demo",
			wantErr: []string{"br start", "--state-dir"},
		},
		{
			name: "boot names the instance in its argument", cmd: bootCmd, flag: "demo",
			wantErr: []string{"br boot"},
		},
		{
			name: "watch spans the whole Mac", cmd: watchCmd, flag: "demo",
			wantErr: []string{"br watch"},
		},
		{
			name: "a subcommand may refuse what its parent honors", cmd: webUntrustCmd, flag: "demo",
			wantErr: []string{"br web untrust", "keychain"},
		},
		{
			name: "a verb that never touches a VM", cmd: disksCmd, flag: "demo",
			wantErr: []string{"br disks"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			selectInstance(t, tc.flag)
			err := checkInstanceFlag(tc.cmd, nil)
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("checkInstanceFlag(%q) returned error: %v", tc.cmd.CommandPath(), err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkInstanceFlag(%q) returned no error, want a refusal", tc.cmd.CommandPath())
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("checkInstanceFlag(%q) error = %q, want it to contain %q", tc.cmd.CommandPath(), err, want)
				}
			}
		})
	}
}

// A BLADERUNNER_INSTANCE left in the environment must never make an unrelated
// command fail: only the flag itself is a deliberate act on this invocation.
func TestCheckInstanceFlagIgnoresTheEnvironment(t *testing.T) {
	t.Setenv(instanceEnvVar, "demo")
	selectInstance(t, "")

	if err := checkInstanceFlag(disksCmd, nil); err != nil {
		t.Errorf("checkInstanceFlag with only %s set returned error: %v", instanceEnvVar, err)
	}
}

// The guard has to be wired to the root command, or every declaration above is
// decoration.
func TestRootRunsTheInstanceFlagGuard(t *testing.T) {
	if rootCmd.PersistentPreRunE == nil {
		t.Fatal("rootCmd.PersistentPreRunE is nil; the --instance guard never runs")
	}
	selectInstance(t, "demo")
	if err := rootCmd.PersistentPreRunE(disksCmd, nil); err == nil {
		t.Error("the root guard accepted --instance for 'br disks'")
	}
}
