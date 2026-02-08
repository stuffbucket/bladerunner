package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"
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
	configDir, err := sshConfigDir()
	if err != nil {
		return err
	}
	configPath := filepath.Join(configDir, "config")

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, errorf("VM not running or not configured"))
		fmt.Fprintln(os.Stderr, subtle("Start it with: br start"))
		return nil
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

func sshConfigDir() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "bladerunner", "ssh"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "bladerunner", "ssh"), nil
}
