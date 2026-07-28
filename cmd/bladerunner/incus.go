package main

import (
	"os"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/ssh"
)

// incusBinary is the guest-side program `br incus` drives.
const incusBinary = "incus"

var incusCmd = &cobra.Command{
	Use:   "incus [args...]",
	Short: "Run incus commands in the VM",
	Long:  `Execute incus commands inside the Bladerunner VM. All arguments are passed to the incus command in the VM.`,
	// Every argument belongs to the guest's incus, so cobra must not read them.
	// passthroughSetup recovers the bladerunner flags cobra therefore skips.
	DisableFlagParsing: true,
	RunE:               runIncus,
}

func runIncus(cmd *cobra.Command, args []string) error {
	guestArgs, handled, err := passthroughSetup(cmd, args)
	if handled || err != nil {
		return err
	}
	if err := rejectJSONForInteractive("incus"); err != nil {
		return err
	}

	configPath, instanceName, err := sshTarget()
	if err != nil {
		return err
	}

	sshPath, sshExecArgs, err := sshArgvFor(ssh.HostAlias(instanceName), configPath, nil, incusCommand(guestArgs)...)
	if err != nil {
		return err
	}

	return syscall.Exec(sshPath, sshExecArgs, os.Environ())
}

// incusCommand returns the command line `br incus` runs in the guest: the incus
// binary followed by every passthrough argument, untouched. `br incus list
// --format json` must reach the guest with --format intact, so nothing here may
// inspect or rewrite these words.
func incusCommand(args []string) []string {
	return append([]string{incusBinary}, args...)
}
