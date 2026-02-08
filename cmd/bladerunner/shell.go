package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/ssh"
)

var shellCmd = &cobra.Command{
	Use:                "shell [-- command...]",
	Short:              "Open an interactive shell in the VM",
	Long:               `Open an interactive shell in the running Bladerunner VM. Any arguments after -- are run as a command.`,
	DisableFlagParsing: true,
	RunE:               runShell,
}

func runShell(cmd *cobra.Command, args []string) error {
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

	// Find SSH config
	configPath := ssh.ConfigPath()

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, errorf("VM not running or not configured"))
		fmt.Fprintln(os.Stderr, subtle("Start it with: br start"))
		return fmt.Errorf("VM not configured")
	}

	// Build ssh command with -t for PTY
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh not found: %w", err)
	}

	sshExecArgs := []string{"ssh", "-t", "-F", configPath, "bladerunner"}
	if len(shellArgs) > 0 {
		sshExecArgs = append(sshExecArgs, shellArgs...)
	}

	return syscall.Exec(sshPath, sshExecArgs, os.Environ())
}
