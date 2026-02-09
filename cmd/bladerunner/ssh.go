package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

var sshCmd = &cobra.Command{
	Use:   "ssh",
	Short: "Show SSH connection details",
	Long:  `Display the SSH command and configuration needed to connect to the Bladerunner VM.`,
	RunE:  runSSH,
}

func runSSH(_ *cobra.Command, _ []string) error {
	configPath, err := sshConfigFromControl()
	if err != nil {
		return err
	}

	fmt.Printf("ssh -F %s bladerunner\n", configPath)
	return nil
}
