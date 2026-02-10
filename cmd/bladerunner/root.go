package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

var rootCmd = &cobra.Command{
	Use:   "br",
	Short: "Bladerunner - Run Incus VMs on macOS",
	Long: `Bladerunner runs Linux VMs on macOS using the Virtualization framework.
It provides a full Incus container environment inside the VM.`,
	Version: version,
	CompletionOptions: cobra.CompletionOptions{
		HiddenDefaultCmd: true,
	},
}

func init() {
	rootCmd.SetVersionTemplate(fmt.Sprintf("br version %s (commit: %s, built: %s)\n", version, commit, date))
	rootCmd.AddCommand(startCmd)
	rootCmd.AddCommand(stopCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(sshCmd)
	rootCmd.AddCommand(shellCmd)
	rootCmd.AddCommand(incusCmd)
	rootCmd.AddCommand(configCmd)
	rootCmd.AddCommand(resetCmd)
}
