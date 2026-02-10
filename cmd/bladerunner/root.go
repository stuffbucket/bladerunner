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
It provides a full Incus container environment inside the VM.`,
	Version: version,
	CompletionOptions: cobra.CompletionOptions{
		HiddenDefaultCmd: true,
	},
}

func init() {
	rootCmd.SetVersionTemplate(fmt.Sprintf("br version %s (commit: %s, built: %s)\n", version, commit, date))

	// Prepend the gradient banner to help output when running interactively.
	defaultHelp := rootCmd.HelpTemplate()
	rootCmd.SetHelpTemplate("{{banner}}" + defaultHelp)
	cobra.AddTemplateFunc("banner", ui.Banner)
	rootCmd.AddCommand(startCmd)
	rootCmd.AddCommand(stopCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(sshCmd)
	rootCmd.AddCommand(shellCmd)
	rootCmd.AddCommand(incusCmd)
	rootCmd.AddCommand(configCmd)
	rootCmd.AddCommand(resetCmd)
	rootCmd.AddCommand(noticeCmd)
}
