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
	Use:                "ssh [vm-name] [-- ssh-args...]",
	Short:              "SSH into a running VM",
	Long:               `Connect to a running VM via SSH. Any arguments after -- are passed to ssh.`,
	DisableFlagParsing: true,
	RunE:               runSSH,
}

func runSSH(cmd *cobra.Command, args []string) error {
	vmName := "incus-vm"

	// Parse args: [vm-name] [-- ssh-args...]
	var sshArgs []string
	for i, arg := range args {
		if arg == "--" {
			sshArgs = args[i+1:]
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

	// Build ssh command
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh not found: %w", err)
	}

	sshExecArgs := make([]string, 0, 4+len(sshArgs))
	sshExecArgs = append(sshExecArgs, "ssh", "-F", configPath, vmName)
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
