package main

import (
	"fmt"

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
	rootCmd.SetVersionTemplate(fmt.Sprintf("br version %s (commit: %s, built: %s)\n", version, commit, date))

	// Prepend the gradient banner to help output when running interactively.
	defaultHelp := rootCmd.HelpTemplate()
	rootCmd.SetHelpTemplate("{{banner}}" + defaultHelp)
	cobra.AddTemplateFunc("banner", ui.Banner)

	// Global --json flag: commands emit machine-readable JSON for agents.
	rootCmd.PersistentFlags().BoolVar(&jsonOutput, "json", false, "Output in JSON format (for scripting/agents)")

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
		upCmd, startCmd, stopCmd, bootCmd, ejectCmd,
		saveCmd, restoreCmd, resetCmd, upgradeCmd, reconnectCmd,
	)
	addToGroup(groupAccess,
		sshCmd, shellCmd, execCmd, incusCmd, lsCmd, logsCmd, eventsCmd,
	)
	addToGroup(groupMedia,
		diskCmd, disksCmd,
	)
	addToGroup(groupUI,
		webCmd, menubarCmd,
	)
	addToGroup(groupConfig,
		statusCmd, configCmd, userCmd, noticeCmd,
	)

	// With groups defined, the built-in help/completion commands would otherwise
	// land in a stray "Additional Commands" bucket. Pin them to Config & Info.
	// (completion is hidden via CompletionOptions, but still needs a group so it
	// doesn't force the extra bucket to render.)
	rootCmd.SetHelpCommandGroupID(groupConfig)
	rootCmd.SetCompletionCommandGroupID(groupConfig)
}
