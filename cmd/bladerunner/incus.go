package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/spf13/cobra"
)

var incusCmd = &cobra.Command{
	Use:                "incus [args...]",
	Short:              "Run incus commands in the VM",
	Long:               `Execute incus commands inside the Bladerunner VM. All arguments are passed to the incus command in the VM.`,
	DisableFlagParsing: true,
	RunE:               runIncus,
}

func runIncus(cmd *cobra.Command, args []string) error {
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			return cmd.Help()
		}
	}

	configPath, err := sshConfigFromControl()
	if err != nil {
		return err
	}

	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh not found: %w", err)
	}

	sshExecArgs := make([]string, 0, 5+len(args))
	sshExecArgs = append(sshExecArgs, "ssh", "-F", configPath, "bladerunner", "incus")
	sshExecArgs = append(sshExecArgs, args...)

	return syscall.Exec(sshPath, sshExecArgs, os.Environ())
}
