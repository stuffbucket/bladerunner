package main

import (
	"fmt"
	"os/exec"

	"github.com/spf13/cobra"
)

// sshHostAlias is the SSH host alias written into the generated ssh config
// (see internal/ssh/config.go "Host bladerunner"); the CLI connects via it.
const sshHostAlias = "bladerunner"

// sshArgv resolves the ssh binary and builds the argument vector the CLI uses
// to reach the VM: "ssh -F <configPath> <opts...> <sshHostAlias> <tail...>".
// argv[0] is "ssh" (as syscall.Exec requires); callers that shell out via
// exec.Command* should pass argv[1:]. This is the single builder shared by the
// shell/incus/reconnect commands so they agree on the -F/host wiring.
func sshArgv(configPath string, opts []string, tail ...string) (sshPath string, argv []string, err error) {
	sshPath, err = exec.LookPath("ssh")
	if err != nil {
		return "", nil, fmt.Errorf("ssh not found: %w", err)
	}
	argv = make([]string, 0, 4+len(opts)+len(tail))
	argv = append(argv, "ssh", "-F", configPath)
	argv = append(argv, opts...)
	argv = append(argv, sshHostAlias)
	argv = append(argv, tail...)
	return sshPath, argv, nil
}

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
