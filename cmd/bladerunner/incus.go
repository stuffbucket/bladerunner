package main

import (
	"os"
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
	if err := rejectJSONForInteractive("incus"); err != nil {
		return err
	}

	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			return cmd.Help()
		}
	}

	configPath, err := sshConfigFromControl()
	if err != nil {
		return err
	}

	sshPath, sshExecArgs, err := sshArgv(configPath, nil, append([]string{"incus"}, args...)...)
	if err != nil {
		return err
	}

	return syscall.Exec(sshPath, sshExecArgs, os.Environ())
}
