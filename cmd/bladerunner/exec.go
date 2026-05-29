package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/incus"
	"golang.org/x/term"
)

var execFlags struct {
	stdin bool
	tty   bool
}

var execCmd = &cobra.Command{
	Use:   "exec <instance> -- <cmd>...",
	Short: "Execute a command inside an Incus instance",
	Long: `Execute a command inside the named Incus instance. Use -- to separate
br flags from the command. Examples:

  br exec mybox -- ls /
  br exec -i -t mybox -- /bin/bash`,
	Args:              cobra.MinimumNArgs(2),
	RunE:              runExec,
	ValidArgsFunction: instanceNameCompletion,
}

func init() {
	execCmd.Flags().BoolVarP(&execFlags.stdin, "stdin", "i", false, "Forward stdin to the remote process")
	execCmd.Flags().BoolVarP(&execFlags.tty, "tty", "t", false, "Allocate a pseudo-TTY (interactive)")
}

// exitError carries the remote exit code so the root command can set the process status.
type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func runExec(cmdCobra *cobra.Command, args []string) error {
	instance := args[0]
	cmd := args[1:]
	if len(cmd) == 0 {
		return errors.New("no command specified after instance name")
	}

	// From here on we propagate exit codes via exitError; suppress cobra's
	// default error/usage rendering so the user only sees their own program's output.
	cmdCobra.SilenceErrors = true
	cmdCobra.SilenceUsage = true

	client, err := connectIncus()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		return &exitError{code: 1}
	}

	opts := incus.ExecOptions{
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
		Interactive: execFlags.tty,
	}
	if execFlags.stdin || execFlags.tty {
		opts.Stdin = os.Stdin
	}

	restore := configureTTY(&opts)
	defer restore()

	exitCode, err := client.ExecInstance(context.Background(), instance, cmd, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: exec %s: %v\n", instance, err)
		return &exitError{code: 1}
	}
	if exitCode != 0 {
		return &exitError{code: exitCode}
	}
	return nil
}

// configureTTY sets opts.Width/Height when running with --tty and puts the local terminal
// into raw mode. It returns a function the caller must defer to restore terminal state.
func configureTTY(opts *incus.ExecOptions) func() {
	noop := func() {}
	if !execFlags.tty {
		return noop
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return noop
	}
	if w, h, err := term.GetSize(fd); err == nil {
		opts.Width = w
		opts.Height = h
	}
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return noop
	}
	return func() { _ = term.Restore(fd, oldState) }
}
