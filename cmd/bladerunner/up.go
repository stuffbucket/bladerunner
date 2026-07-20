package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/control"
)

// upCmd is the single memorable entry verb. It brings a VM up with sensible
// defaults and prints a short "what now" block, or — when a VM is already
// running — reports that and points at the same next steps. It intentionally
// reuses runStart (the explicit, flag-rich form) rather than duplicating boot
// logic; `br start` stays the way to pass --cpus/--memory/--image-url/etc.
var upCmd = &cobra.Command{
	Use:   "up",
	Short: "Bring a VM up (start with defaults, then show next steps)",
	Long: `Bring a Bladerunner VM up with sensible defaults.

If a VM is already running, up prints its status and the next steps. Otherwise
it starts one (equivalent to 'br start' with no flags) and then shows how to get
into it. Use 'br start' directly when you need to pass CPU/memory/image flags.`,
	Args: cobra.NoArgs,
	RunE: runUp,
}

func runUp(cmd *cobra.Command, args []string) error {
	// If a VM is already running, don't try to start a second one (runStart
	// would error) — just report and point at the next steps.
	if cfg, err := config.Default(startFlags.stateDir); err == nil {
		if control.NewClient(cfg.VMDir).IsRunning() {
			if jsonOutput {
				return emitJSON(map[string]string{jsonFieldStatus: "already-running"})
			}
			fmt.Println()
			fmt.Println(success("✓ VM is already running"))
			printNextSteps()
			return nil
		}
	}

	// Delegate to the real start path with its defaults. runStart prints its own
	// running summary (SSH/Shell/API); we append the broader "what now" block so
	// `br web` and `br ls` are one glance away, per issue #131.
	if err := runStart(cmd, args); err != nil {
		return err
	}
	if !jsonOutput {
		printNextSteps()
	}
	return nil
}

// printNextSteps lists the handful of verbs a user reaches for right after a VM
// comes up. Kept small on purpose — start.go's printRunningSummary already
// covers SSH/Shell/API; this adds the "explore" verbs (#131).
func printNextSteps() {
	fmt.Println(subtle("  What now:"))
	fmt.Printf("    %s  %s\n", command("br shell"), subtle("open an interactive shell"))
	fmt.Printf("    %s    %s\n", command("br web"), subtle("open the Incus web UI"))
	fmt.Printf("    %s     %s\n", command("br ls"), subtle("list Incus instances"))
	fmt.Println()
}
