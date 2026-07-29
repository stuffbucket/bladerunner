package main

import (
	"slices"
	"strings"
	"testing"
)

// `br shell` and `br incus` disable cobra's flag parsing so the guest's own
// flags survive. That also silenced the root's persistent flags, so this pins
// the boundary exactly: a bladerunner flag counts only while it LEADS the
// argument list, and everything from the first other token on is the guest's.
func TestSplitPassthroughArgs(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantOpts passthroughOpts
		wantRest []string
		wantErr  string
	}{
		{name: "no arguments"},
		{
			name:     "instance flag ahead of the boundary",
			args:     []string{"--instance", "foo", "--", "true"},
			wantOpts: passthroughOpts{Instance: "foo"},
			wantRest: []string{"--", "true"},
		},
		{
			name:     "instance flag written with an equals sign",
			args:     []string{"--instance=foo", "list"},
			wantOpts: passthroughOpts{Instance: "foo"},
			wantRest: []string{"list"},
		},
		{
			name:     "json flag",
			args:     []string{"--json", "--instance", "foo", "list"},
			wantOpts: passthroughOpts{Instance: "foo", JSON: true},
			wantRest: []string{"list"},
		},
		{
			name:     "json flag switched off explicitly",
			args:     []string{"--json=false", "list"},
			wantRest: []string{"list"},
		},
		{
			name:     "a guest subcommand closes the boundary",
			args:     []string{"list", "--format", "json"},
			wantRest: []string{"list", "--format", "json"},
		},
		{
			name:     "a leading guest flag closes the boundary",
			args:     []string{"--project", "p", "list"},
			wantRest: []string{"--project", "p", "list"},
		},
		{
			name:     "a bladerunner flag after the boundary belongs to the guest",
			args:     []string{"list", "--instance", "foo"},
			wantRest: []string{"list", "--instance", "foo"},
		},
		{
			name:     "an explicit boundary is kept for the verb to read",
			args:     []string{"--", "ls", "-la"},
			wantRest: []string{"--", "ls", "-la"},
		},
		{
			name:     "help",
			args:     []string{"--help"},
			wantOpts: passthroughOpts{Help: true},
		},
		{
			name:     "short help",
			args:     []string{"-h"},
			wantOpts: passthroughOpts{Help: true},
		},
		{
			name:     "help only counts while it leads",
			args:     []string{"list", "--help"},
			wantRest: []string{"list", "--help"},
		},
		{
			name:    "instance flag with no value",
			args:    []string{"--instance"},
			wantErr: "flag needs an argument: --instance",
		},
		{
			name:    "json flag with a value that is not a boolean",
			args:    []string{"--json=maybe"},
			wantErr: `invalid argument "maybe"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, rest, err := splitPassthroughArgs(tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("splitPassthroughArgs(%q) error = %v, want it to contain %q", tc.args, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("splitPassthroughArgs(%q) returned error: %v", tc.args, err)
			}
			if opts != tc.wantOpts {
				t.Errorf("splitPassthroughArgs(%q) opts = %+v, want %+v", tc.args, opts, tc.wantOpts)
			}
			if !slices.Equal(rest, tc.wantRest) {
				t.Errorf("splitPassthroughArgs(%q) rest = %q, want %q", tc.args, rest, tc.wantRest)
			}
		})
	}
}

// The guest command line is the whole point of the passthrough verbs, so pin
// it: a bladerunner flag must never reach the guest, and a guest flag must
// reach it untouched.
func TestIncusCommand(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{name: "bare", args: nil, want: []string{"incus"}},
		{name: "subcommand", args: []string{"list"}, want: []string{"incus", "list"}},
		{
			name: "a guest flag reaches the guest",
			args: []string{"list", "--format", "json"},
			want: []string{"incus", "list", "--format", "json"},
		},
		{
			name: "a leading guest flag reaches the guest",
			args: []string{"--project", "p", "list"},
			want: []string{"incus", "--project", "p", "list"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := incusCommand(tc.args); !slices.Equal(got, tc.want) {
				t.Errorf("incusCommand(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

// End to end over the split: `br incus list --format json` keeps --format, and
// `br --instance foo incus list` does not leak --instance into the guest.
func TestIncusCommandNeverCarriesABladerunnerFlag(t *testing.T) {
	cases := []struct {
		name         string
		args         []string
		wantGuest    []string
		wantInstance string
	}{
		{
			name:      "guest flag survives",
			args:      []string{"list", "--format", "json"},
			wantGuest: []string{"incus", "list", "--format", "json"},
		},
		{
			name:         "bladerunner flag is consumed",
			args:         []string{"--instance", "foo", "list"},
			wantGuest:    []string{"incus", "list"},
			wantInstance: "foo",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, rest, err := splitPassthroughArgs(tc.args)
			if err != nil {
				t.Fatalf("splitPassthroughArgs(%q) returned error: %v", tc.args, err)
			}
			if opts.Instance != tc.wantInstance {
				t.Errorf("instance = %q, want %q", opts.Instance, tc.wantInstance)
			}
			if got := incusCommand(rest); !slices.Equal(got, tc.wantGuest) {
				t.Errorf("guest command = %q, want %q", got, tc.wantGuest)
			}
		})
	}
}

// `br shell` runs what follows the -- boundary and nothing else; everything
// before it is the CLI's own.
func TestShellCommand(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{name: "interactive", args: nil, want: nil},
		{name: "bare boundary is still interactive", args: []string{"--"}, want: []string{}},
		{name: "command after the boundary", args: []string{"--", "ls", "-la"}, want: []string{"ls", "-la"}},
		{name: "a flag after the boundary is the guest's", args: []string{"--", "sh", "-c", "true"}, want: []string{"sh", "-c", "true"}},
		{name: "no boundary means no command", args: []string{"ls"}, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shellCommand(tc.args); !slices.Equal(got, tc.want) {
				t.Errorf("shellCommand(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

// applyPassthroughOpts must write the same package variables cobra's persistent
// flags are bound to, so the flag keeps beating BLADERUNNER_INSTANCE for a
// passthrough verb exactly as it does everywhere else.
func TestApplyPassthroughOptsBeatsTheEnvironment(t *testing.T) {
	t.Setenv(instanceEnvVar, "fromenv")
	t.Setenv(instanceEnvVarAlias, "")
	saved := instanceFlag
	t.Cleanup(func() { instanceFlag = saved })
	instanceFlag = ""

	if got := selectedInstanceName(); got != "fromenv" {
		t.Fatalf("selectedInstanceName() = %q before the flag is applied, want %q", got, "fromenv")
	}
	applyPassthroughOpts(passthroughOpts{Instance: "fromflag"})
	if got := selectedInstanceName(); got != "fromflag" {
		t.Errorf("selectedInstanceName() = %q after applying --instance, want %q", got, "fromflag")
	}
}

// The passthrough verbs re-implement flag parsing, so a persistent flag added
// to the root command has to be added to passthroughFlags too — otherwise
// `br --newflag shell` starts silently forwarding "--newflag" to the guest,
// which is the whole defect this file exists to prevent.
func TestPassthroughCoversEveryPersistentFlag(t *testing.T) {
	known := make(map[string]bool, len(passthroughFlags))
	for _, name := range passthroughFlags {
		if rootCmd.PersistentFlags().Lookup(name) == nil {
			t.Errorf("passthroughFlags names %q, which the root command does not declare as a persistent flag", name)
		}
		// The list is only a claim; check that splitPassthroughArgs really
		// consumes the flag rather than handing it to the guest.
		args := []string{"--" + name, "value"}
		_, rest, err := splitPassthroughArgs(args)
		if err != nil {
			t.Errorf("splitPassthroughArgs(%q) returned error: %v", args, err)
		} else if len(rest) > 0 && rest[0] == args[0] {
			t.Errorf("splitPassthroughArgs(%q) left %q for the guest; passthroughFlags claims it is consumed", args, rest[0])
		}
		known[name] = true
	}
	for _, name := range rootPersistentFlagNames(t) {
		if !known[name] {
			t.Errorf("root persistent flag --%s is not handled by splitPassthroughArgs; add it to passthroughFlags", name)
		}
	}
}

// rootPersistentFlagNames reads the root command's persistent flag names out of
// its rendered usage. FlagUsages is used rather than VisitAll so this test does
// not have to name a pflag type: pflag is an indirect module dependency and
// importing it directly would change go.mod.
func rootPersistentFlagNames(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, line := range strings.Split(rootCmd.PersistentFlags().FlagUsages(), "\n") {
		for _, field := range strings.Fields(line) {
			if strings.HasPrefix(field, "--") {
				names = append(names, strings.TrimPrefix(field, "--"))
				break
			}
		}
	}
	if len(names) == 0 {
		t.Fatal("read no persistent flag names from the root command's usage")
	}
	return names
}
