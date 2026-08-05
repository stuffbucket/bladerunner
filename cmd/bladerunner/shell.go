package main

import (
	"os"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/ssh"
)

var shellCmd = &cobra.Command{
	Use:   "shell [-- command...]",
	Short: "Open an interactive shell in the VM",
	Long: `Open an interactive shell in the running Bladerunner VM. Any arguments after -- are run as a command.

Also answers to 'br ssh', which is what the same verb is called in colima,
multipass and vagrant. 'br ssh-config' prints the connection details instead.`,
	// ssh is an alias rather than a second command so the two cannot drift.
	// It used to be its own verb that PRINTED connection details, which is why
	// `br ssh --json` now points at `br ssh-config --json`.
	Aliases: []string{"ssh"},
	// The trailing command belongs to the guest, so cobra must not read it.
	// passthroughSetup recovers the bladerunner flags cobra therefore skips.
	DisableFlagParsing: true,
	RunE:               runShell,
}

func runShell(cmd *cobra.Command, args []string) error {
	guestArgs, handled, err := passthroughSetup(cmd, args)
	if handled || err != nil {
		return err
	}
	if err := rejectJSONForInteractive("shell", "br ssh-config --json", "br status --json"); err != nil {
		return err
	}

	configPath, instanceName, err := sshTarget()
	if err != nil {
		return err
	}

	// Build ssh command with -t for PTY
	sshPath, sshExecArgs, err := sshArgvFor(ssh.HostAlias(instanceName), configPath, []string{"-t"}, shellCommand(guestArgs)...)
	if err != nil {
		return err
	}

	return syscall.Exec(sshPath, sshExecArgs, os.Environ())
}

// shellCommand returns what `br shell` runs in the guest: whatever follows the
// -- boundary, and nothing at all when there is no boundary, which is the
// interactive login shell.
func shellCommand(args []string) []string {
	for i, arg := range args {
		if arg == passthroughBoundary {
			return args[i+1:]
		}
	}
	return nil
}
