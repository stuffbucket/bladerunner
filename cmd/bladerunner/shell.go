package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"
)

var shellCmd = &cobra.Command{
	Use:                "shell [vm-name] [-- command...]",
	Short:              "Open an interactive shell in the VM",
	Long:               `Open an interactive shell in a running VM. Any arguments after -- are run as a command.`,
	DisableFlagParsing: true,
	RunE:               runShell,
}

func runShell(cmd *cobra.Command, args []string) error {
	vmName := "incus-vm"

	// Parse args: [vm-name] [-- command...]
	var shellArgs []string
	for i, arg := range args {
		if arg == "--" {
			shellArgs = args[i+1:]
			break
		}
		if arg == "--help" || arg == "-h" {
			return cmd.Help()
		}
		if i == 0 && arg != "" && arg[0] != '-' {
			vmName = arg
		}
	}

	// Find SSH config
	configDir, err := sshConfigDir()
	if err != nil {
		return err
	}
	configPath := filepath.Join(configDir, vmName+".ssh_config")

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, errorf("VM not running or not configured: ")+vmName)
		fmt.Fprintln(os.Stderr, subtle("Start it with: br start --name "+vmName))
		return nil
	}

	// Build ssh command with -t for PTY
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh not found: %w", err)
	}

	sshExecArgs := []string{"ssh", "-t", "-F", configPath, vmName}
	if len(shellArgs) > 0 {
		sshExecArgs = append(sshExecArgs, shellArgs...)
	}

	return syscall.Exec(sshPath, sshExecArgs, os.Environ())
}
