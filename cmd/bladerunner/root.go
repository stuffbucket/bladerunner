package main

import (
	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/ui"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

var rootCmd = &cobra.Command{
	Use:   "br",
	Short: "Bladerunner - Run Incus VMs on macOS",
	Long: `Bladerunner runs Incus VM on macOS using the Apple VZ framework.
It provides a full Incus container environment inside the VM.

Getting started:
  br up       Bring a VM up with defaults, then show next steps
  br shell    Open an interactive shell in the VM
  br web      Open the Incus web UI with single sign-on
  br status   Show VM status`,
	Version: version,
	CompletionOptions: cobra.CompletionOptions{
		HiddenDefaultCmd: true,
	},
}

// The root command's persistent flags, named once. `br shell` and `br incus`
// disable cobra's flag parsing (see passthrough.go) and have to recognize these
// themselves, so the names must not be spelled twice.
const (
	jsonFlagName     = "json"
	instanceFlagName = "instance"
	// helpFlagName is cobra's built-in help flag, which the passthrough verbs
	// also answer themselves.
	helpFlagName = "help"
)

// Command group IDs. Every verb is assigned one of these so `br --help`
// renders as titled buckets instead of one alphabetical wall (issue #131).
const (
	groupLifecycle = "lifecycle"
	groupAccess    = "access"
	groupMedia     = "media"
	groupUI        = "ui"
	groupConfig    = "config"
)

func init() {
	rootCmd.SetVersionTemplate(versionLine() + "\n")

	// Prepend the gradient banner to help output when running interactively.
	defaultHelp := rootCmd.HelpTemplate()
	rootCmd.SetHelpTemplate("{{banner}}" + defaultHelp)
	cobra.AddTemplateFunc("banner", ui.Banner)

	// Global --json flag: commands emit machine-readable JSON for agents.
	rootCmd.PersistentFlags().BoolVar(&jsonOutput, jsonFlagName, false, "Output in JSON format (for scripting/agents)")

	// Global --instance flag: which VM a verb acts on when several are running.
	// With a single running VM it can be omitted and nothing changes; see
	// resolveInstanceTarget. BLADERUNNER_INSTANCE (or BR_INSTANCE) is the
	// environment equivalent.
	rootCmd.PersistentFlags().StringVar(&instanceFlag, instanceFlagName, "",
		"Which VM instance to act on (default: the single running one; env "+instanceEnvVar+")")

	// Titled command buckets for `br --help`. Order here is the display order.
	rootCmd.AddGroup(
		&cobra.Group{ID: groupLifecycle, Title: "Lifecycle:"},
		&cobra.Group{ID: groupAccess, Title: "Access:"},
		&cobra.Group{ID: groupMedia, Title: "Media:"},
		&cobra.Group{ID: groupUI, Title: "UI:"},
		&cobra.Group{ID: groupConfig, Title: "Config & Info:"},
	)

	// addToGroup registers a command under a group so cobra renders it in that
	// bucket. Setting GroupID here (rather than on each command literal) keeps
	// the categorization in one place.
	addToGroup := func(groupID string, cmds ...*cobra.Command) {
		for _, c := range cmds {
			c.GroupID = groupID
			rootCmd.AddCommand(c)
		}
	}

	addToGroup(groupLifecycle,
		upCmd, startCmd, stopCmd, restartCmd, bootCmd, ejectCmd,
		saveCmd, restoreCmd, resetCmd, upgradeCmd, selfUpdateCmd, reconnectCmd,
	)
	addToGroup(groupAccess,
		shellCmd, sshConfigCmd, execCmd, incusCmd, lsCmd, logsCmd, eventsCmd,
	)
	addToGroup(groupMedia,
		diskCmd, disksCmd,
	)
	addToGroup(groupUI,
		webCmd, menubarCmd,
	)
	addToGroup(groupConfig,
		statusCmd, instancesCmd, configCmd, userCmd, noticeCmd, versionCmd,
	)

	// With groups defined, the built-in help/completion commands would otherwise
	// land in a stray "Additional Commands" bucket. Pin them to Config & Info.
	// (completion is hidden via CompletionOptions, but still needs a group so it
	// doesn't force the extra bucket to render.)
	rootCmd.SetHelpCommandGroupID(groupConfig)
	rootCmd.SetCompletionCommandGroupID(groupConfig)

	// --instance policy (issue #9). Cobra shows the flag in every command's
	// help, so every command must answer for it. An UNDECLARED command refuses
	// the flag, which is why only the two accepting policies are listed here.
	// A subcommand inherits its parent unless it declares its own.
	declareInstancePolicy(instanceHonored,
		statusCmd, stopCmd, restartCmd, resetCmd, ejectCmd, saveCmd, restoreCmd, upgradeCmd,
		reconnectCmd, sshConfigCmd, shellCmd, execCmd, incusCmd, lsCmd, logsCmd,
		eventsCmd, webCmd, configCmd,
	)
	// `br instances` lists every instance; selecting one would mean nothing.
	declareInstancePolicy(instanceAllInstances, instancesCmd)
	// `br web untrust` only edits this Mac's keychain, so it refuses the flag
	// its parent honors.
	declareInstancePolicy(instanceRefused, webUntrustCmd)

	// Synonyms a user arriving from another tool will type. Cobra already
	// suggests near-misses by edit distance ("resart" finds restart), but a
	// SYNONYM is not a near-miss: `br delete` is nowhere near `br reset`, so
	// without this it gets "unknown command" and nothing else.
	//
	// These do not create new verbs. `br ls` already means Incus instances and
	// `br instances` means VMs; adding a third spelling of either would trade
	// one confusion for another. A suggestion points and lets the user choose.
	resetCmd.SuggestFor = []string{"delete", "destroy", "rm", "teardown"}
	instancesCmd.SuggestFor = []string{"list"}
	stopCmd.SuggestFor = []string{"halt", "down"}
	shellCmd.SuggestFor = []string{"console", "attach"}

	declareInstanceHint(upCmd, "'br up' brings the default VM up; boot a named instance with 'br boot <name>'")
	declareInstanceHint(startCmd, "'br start' creates an instance; choose where with --state-dir, or boot a named one with 'br boot <name>'")
	declareInstanceHint(bootCmd, "'br boot' names the instance it creates in its own argument: 'br boot <name>'")
	declareInstanceHint(watchCmd, "'br watch' spans every cartridge inserted on this Mac")
	declareInstanceHint(webUntrustCmd, "'br web untrust' only edits this Mac's keychain, which no instance owns")

	// The guard itself. Nothing else defines a PersistentPreRunE, so cobra runs
	// this one for every command at every depth.
	rootCmd.PersistentPreRunE = checkInstanceFlag
}
