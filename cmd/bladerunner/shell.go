package main

import (
	"os"
	"syscall"

	"github.com/spf13/cobra"
)

var shellCmd = &cobra.Command{
	Use:                "shell [-- command...]",
	Short:              "Open an interactive shell in the VM",
	Long:               `Open an interactive shell in the running Bladerunner VM. Any arguments after -- are run as a command.`,
	DisableFlagParsing: true,
	RunE:               runShell,
}

func runShell(cmd *cobra.Command, args []string) error {
	if err := rejectJSONForInteractive("shell"); err != nil {
		return err
	}

	// Parse args: [-- command...]
	var shellArgs []string
	for i, arg := range args {
		if arg == "--" {
			shellArgs = args[i+1:]
			break
		}
		if arg == "--help" || arg == "-h" {
			return cmd.Help()
		}
	}

	configPath, err := sshConfigFromControl()
	if err != nil {
		return err
	}

	// Build ssh command with -t for PTY
	sshPath, sshExecArgs, err := sshArgv(configPath, []string{"-t"}, shellArgs...)
	if err != nil {
		return err
	}

	return syscall.Exec(sshPath, sshExecArgs, os.Environ())
}
