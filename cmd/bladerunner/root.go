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
	Use:   "runner",
	Short: "Bladerunner - Run Incus VMs on macOS",
	Long: `Bladerunner runs Incus VM on macOS using the Apple VZ framework.
It provides a full Incus container environment inside the VM.`,
	Version: version,
	CompletionOptions: cobra.CompletionOptions{
		HiddenDefaultCmd: true,
	},
}

func init() {
	rootCmd.SetVersionTemplate(fmt.Sprintf("runner version %s (commit: %s, built: %s)\n", version, commit, date))

	// Prepend the gradient banner to help output when running interactively.
	defaultHelp := rootCmd.HelpTemplate()
	rootCmd.SetHelpTemplate("{{banner}}" + defaultHelp)
	cobra.AddTemplateFunc("banner", ui.Banner)

	// Global --json flag: commands emit machine-readable JSON for agents.
	rootCmd.PersistentFlags().BoolVar(&jsonOutput, "json", false, "Output in JSON format (for scripting/agents)")

	rootCmd.AddCommand(startCmd)
	rootCmd.AddCommand(stopCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(saveCmd)
	rootCmd.AddCommand(restoreCmd)
	rootCmd.AddCommand(upgradeCmd)
	rootCmd.AddCommand(sshCmd)
	rootCmd.AddCommand(shellCmd)
	rootCmd.AddCommand(incusCmd)
	rootCmd.AddCommand(lsCmd)
	rootCmd.AddCommand(execCmd)
	rootCmd.AddCommand(logsCmd)
	rootCmd.AddCommand(eventsCmd)
	rootCmd.AddCommand(configCmd)
	rootCmd.AddCommand(resetCmd)
	rootCmd.AddCommand(noticeCmd)
	rootCmd.AddCommand(userCmd)
	rootCmd.AddCommand(webCmd)
}
