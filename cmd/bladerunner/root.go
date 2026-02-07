package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

var version = "dev"

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
	rootCmd.SetVersionTemplate(fmt.Sprintf("br version %s\n", version))
	rootCmd.AddCommand(startCmd)
	rootCmd.AddCommand(sshCmd)
	rootCmd.AddCommand(shellCmd)
}
