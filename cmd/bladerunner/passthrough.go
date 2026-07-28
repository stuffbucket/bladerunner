package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// The passthrough verbs — `br shell` and `br incus` — hand their trailing
// arguments to a program inside the guest. Cobra must not interpret those:
// `br incus list --format json` has to arrive in the guest with --format
// intact, and cobra would reject it as an unknown flag. Both commands therefore
// set DisableFlagParsing.
//
// DisableFlagParsing also skips the ROOT's persistent flags, so --instance and
// --json were never parsed for these two verbs (issue #9). They arrived as
// ordinary words: `br --instance foo incus list` ran `incus --instance foo list`
// inside the guest, and `br shell` could not obey its own "select one with
// --instance <name>" advice.
//
// This file re-implements the small part of the parsing those verbs do want. A
// bladerunner flag is recognized only while it LEADS the argument list. The
// first token that is not one closes the boundary, and everything from there on
// belongs to the guest, flags included.

const (
	// passthroughBoundary ends the bladerunner flags explicitly. It stays in the
	// guest arguments, because `br shell -- ls` has always meant "run ls" and
	// the shell verb reads the boundary itself.
	passthroughBoundary = "--"

	// longFlagPrefix is what a long flag starts with.
	longFlagPrefix = "--"

	// shortHelpFlag is the one-letter spelling of --help.
	shortHelpFlag = "-h"
)

// passthroughOpts is the bladerunner half of a split passthrough command line.
type passthroughOpts struct {
	// Instance is the --instance value, empty when it was not given.
	Instance string
	// JSON records --json.
	JSON bool
	// Help records --help or -h, which the verb answers itself rather than
	// forwarding to the guest.
	Help bool
}

// passthroughFlags lists the bladerunner flags a passthrough verb accepts ahead
// of the guest's own arguments. It must name every persistent flag the root
// command declares; TestPassthroughCoversEveryPersistentFlag enforces that.
var passthroughFlags = []string{instanceFlagName, jsonFlagName}

// splitPassthroughArgs separates the leading bladerunner flags from the
// arguments that belong to the guest.
//
// The split is deliberately positional. Only a LEADING run of known bladerunner
// flags is consumed, so `br incus --project p list` still reaches the guest
// whole and `br incus list --format json` keeps its --format: the first token
// that is not a bladerunner flag closes the boundary for good.
func splitPassthroughArgs(args []string) (passthroughOpts, []string, error) {
	var opts passthroughOpts
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == passthroughBoundary {
			return opts, args[i:], nil
		}
		if arg == longFlagPrefix+helpFlagName || arg == shortHelpFlag {
			opts.Help = true
			return opts, args[i+1:], nil
		}

		name, inline, hasInline := strings.Cut(arg, "=")
		switch name {
		case longFlagPrefix + instanceFlagName:
			value, last, err := passthroughFlagValue(name, args, i, inline, hasInline)
			if err != nil {
				return passthroughOpts{}, nil, err
			}
			opts.Instance, i = strings.TrimSpace(value), last
		case longFlagPrefix + jsonFlagName:
			on, err := passthroughBoolValue(name, inline, hasInline)
			if err != nil {
				return passthroughOpts{}, nil, err
			}
			opts.JSON = on
		default:
			return opts, args[i:], nil
		}
	}
	return opts, nil, nil
}

// passthroughFlagValue reads the value of a flag that takes one, either from
// --flag=value or from the argument that follows. last is the index of the last
// token the flag consumed.
func passthroughFlagValue(name string, args []string, i int, inline string, hasInline bool) (value string, last int, err error) {
	if hasInline {
		return inline, i, nil
	}
	if i+1 >= len(args) {
		return "", 0, fmt.Errorf("flag needs an argument: %s", name)
	}
	return args[i+1], i + 1, nil
}

// passthroughBoolValue reads a boolean flag, which is true when written bare
// and takes an explicit value only in the --flag=value form (the bare form must
// not swallow the guest's next word).
func passthroughBoolValue(name, inline string, hasInline bool) (bool, error) {
	if !hasInline {
		return true, nil
	}
	on, err := strconv.ParseBool(inline)
	if err != nil {
		return false, fmt.Errorf("invalid argument %q for %q flag: %w", inline, name, err)
	}
	return on, nil
}

// applyPassthroughOpts writes the parsed flags into the same package variables
// the root command's persistent flags are bound to (root.go), so
// selectedInstanceName and the --json guards see exactly what cobra would have
// given them. The flag therefore keeps beating BLADERUNNER_INSTANCE, because
// selectedInstanceName reads instanceFlag first.
func applyPassthroughOpts(opts passthroughOpts) {
	if opts.Instance != "" {
		instanceFlag = opts.Instance
	}
	if opts.JSON {
		jsonOutput = true
	}
}

// passthroughSetup parses the leading bladerunner flags of a passthrough verb,
// applies them, and returns the arguments meant for the guest. handled reports
// that the verb has already answered the invocation (--help), so its RunE must
// return without contacting a VM.
func passthroughSetup(cmd *cobra.Command, args []string) (guestArgs []string, handled bool, err error) {
	opts, rest, err := splitPassthroughArgs(args)
	if err != nil {
		return nil, false, err
	}
	applyPassthroughOpts(opts)
	if opts.Help {
		return nil, true, cmd.Help()
	}
	return rest, false, nil
}
