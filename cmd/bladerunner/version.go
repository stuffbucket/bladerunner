package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// versionLine is the one place the version string is formatted.
//
// `br --version` and `br version` must not drift into two spellings of the same
// fact, which is what happens when a subcommand is added beside an existing
// flag and formats its own output.
func versionLine() string {
	return fmt.Sprintf("br version %s (commit: %s, built: %s)", version, commit, date)
}

// versionCmd prints the version.
//
// The --version flag already did this. The subcommand exists because that is
// how every comparable tool spells it — `colima version`, `docker version`,
// `kubectl version` — and a user who types `br version` should not be told it
// is an unknown command when the tool plainly knows its own version.
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the bladerunner version",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		// OutOrStdout, not cmd.Println: cobra's Println writes to OutOrStderr,
		// so `br version | cat` would print nothing. A version is the most
		// likely thing in this CLI to be captured by a script.
		fmt.Fprintln(cmd.OutOrStdout(), versionLine())
		return nil
	},
}
