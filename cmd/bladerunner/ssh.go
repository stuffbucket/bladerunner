package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/ssh"
)

var sshCmd = &cobra.Command{
	Use:                "ssh [-- ssh-args...]",
	Short:              "SSH into the running VM",
	Long:               `Connect to the running Bladerunner VM via SSH. Any arguments after -- are passed to ssh.`,
	DisableFlagParsing: true,
	RunE:               runSSH,
}

func runSSH(cmd *cobra.Command, args []string) error {
	// Parse args: [-- ssh-args...]
	var sshArgs []string
	for i, arg := range args {
		if arg == "--" {
			sshArgs = args[i+1:]
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

	// Build ssh command
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh not found: %w", err)
	}

	sshExecArgs := make([]string, 0, 4+len(sshArgs))
	sshExecArgs = append(sshExecArgs, "ssh", "-F", configPath, "bladerunner")
	sshExecArgs = append(sshExecArgs, sshArgs...)

	return syscall.Exec(sshPath, sshExecArgs, os.Environ())
}
