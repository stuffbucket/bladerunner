package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// sshHostAlias is the SSH host alias written into the generated ssh config
// (see internal/ssh/config.go "Host bladerunner"); the CLI connects via it.
const sshHostAlias = "bladerunner"

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
			"host":            sshHostAlias,
			"command":         fmt.Sprintf("ssh -F %s %s", configPath, sshHostAlias),
		})
	}

	fmt.Printf("ssh -F %s %s\n", configPath, sshHostAlias)
	return nil
}
