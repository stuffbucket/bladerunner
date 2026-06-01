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
		if jsonOutput {
			emitJSONError(err)
		}
		return err
	}

	if jsonOutput {
		return emitJSON(map[string]string{
			"ssh_config_path": configPath,
			"host":            "bladerunner",
			"command":         fmt.Sprintf("ssh -F %s bladerunner", configPath),
		})
	}

	fmt.Printf("ssh -F %s bladerunner\n", configPath)
	return nil
}
