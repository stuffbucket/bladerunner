package main

import (
	"fmt"
	"os/exec"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/ssh"
)

// sshArgvFor resolves the ssh binary and builds the argument vector the CLI
// uses to reach an instance: "ssh -F <configPath> <opts...> <alias> <tail...>".
// argv[0] is "ssh" (as syscall.Exec requires); callers that shell out via
// exec.Command* should pass argv[1:]. This is the single builder shared by the
// shell/incus/reconnect commands so they agree on the -F/host wiring.
//
// There is deliberately no shorthand that fills in the DEFAULT instance's
// alias: every caller resolves its alias from sshTarget, so pointing a verb at
// --instance <name> reaches that instance's own config.d fragment (issue #9).
func sshArgvFor(alias, configPath string, opts []string, tail ...string) (sshPath string, argv []string, err error) {
	sshPath, err = exec.LookPath("ssh")
	if err != nil {
		return "", nil, fmt.Errorf("ssh not found: %w", err)
	}
	argv = make([]string, 0, 4+len(opts)+len(tail))
	argv = append(argv, "ssh", "-F", configPath)
	argv = append(argv, opts...)
	argv = append(argv, alias)
	argv = append(argv, tail...)
	return sshPath, argv, nil
}

// sshConfigCmd prints how to reach the VM over SSH.
//
// This verb was called `br ssh` and did exactly this. It was renamed because
// every comparable tool spells the CONNECTING verb `ssh` — colima, multipass,
// vagrant — so `br ssh` printing text and leaving you where you started was a
// surprise every new user paid for once. `ssh-config` is the name colima gives
// the same output, so the pair reads the way people already expect.
var sshConfigCmd = &cobra.Command{
	Use:     "ssh-config",
	Short:   "Show SSH connection details",
	Long:    `Display the SSH command and configuration needed to connect to the Bladerunner VM.`,
	Aliases: []string{"ssh-info"},
	Args:    cobra.NoArgs,
	RunE:    runSSHConfig,
}

func runSSHConfig(_ *cobra.Command, _ []string) error {
	configPath, instanceName, err := sshTarget()
	if err != nil {
		if jsonOutput {
			emitJSONError(err)
		}
		return err
	}
	alias := ssh.HostAlias(instanceName)

	if jsonOutput {
		return emitJSON(map[string]string{
			"ssh_config_path": configPath,
			"host":            alias,
			"command":         ssh.CommandFor(configPath, instanceName),
		})
	}

	fmt.Println(ssh.CommandFor(configPath, instanceName))
	return nil
}
