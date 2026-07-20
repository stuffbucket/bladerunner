package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/update"
)

// selfUpdateFlags holds the flags for `br self-update`.
var selfUpdateFlags struct {
	check       bool
	manifestURL string
	noRelaunch  bool
}

var selfUpdateCmd = &cobra.Command{
	Use:   "self-update",
	Short: "Update a .dmg-installed Bladerunner.app in place",
	Long: `Download and install the latest signed Bladerunner.app over the current
one, verifying its Ed25519 signature before replacing anything.

This applies only to the signed .dmg install channel. Homebrew installs are
detected and refused — run 'brew upgrade bladerunner' for those. This is
distinct from 'br upgrade', which hands the running control server to a new
binary already on disk; 'br self-update' fetches and installs a new binary.

With --check, only compare versions and report whether an update is available;
nothing is downloaded or modified.`,
	RunE: runSelfUpdate,
}

func init() {
	selfUpdateCmd.Flags().BoolVar(&selfUpdateFlags.check, "check", false, "Only check for an update; do not download or install")
	selfUpdateCmd.Flags().StringVar(&selfUpdateFlags.manifestURL, "manifest", "", "Override the update manifest URL")
	selfUpdateCmd.Flags().BoolVar(&selfUpdateFlags.noRelaunch, "no-relaunch", false, "Do not relaunch the app after a successful update")
	rootCmd.AddCommand(selfUpdateCmd)
}

func runSelfUpdate(cmd *cobra.Command, _ []string) error {
	opts := update.Options{
		CurrentVersion: version,
		ManifestURL:    selfUpdateFlags.manifestURL,
	}

	if selfUpdateFlags.check {
		return runSelfUpdateCheck(cmd.Context(), opts)
	}
	return runSelfUpdateApply(cmd.Context(), opts)
}

func runSelfUpdateCheck(ctx context.Context, opts update.Options) error {
	res, err := update.Check(ctx, opts)
	if err != nil {
		return jsonOrError(err)
	}
	if jsonOutput {
		return emitJSON(map[string]any{
			jsonFieldStatus:    checkStatus(res.UpdateAvailable),
			"current":          res.CurrentVersion,
			"latest":           res.LatestVersion,
			"update_available": res.UpdateAvailable,
		})
	}
	if res.UpdateAvailable {
		fmt.Printf("%s Update available: %s → %s\n", success("✓"), subtle(res.CurrentVersion), value(res.LatestVersion))
		fmt.Printf("Run %s to install.\n", command("br self-update"))
		if res.Notes != "" {
			fmt.Printf("\n%s\n", res.Notes)
		}
		return nil
	}
	fmt.Printf("%s Already up to date (%s)\n", success("✓"), value(res.CurrentVersion))
	return nil
}

func runSelfUpdateApply(ctx context.Context, opts update.Options) error {
	opts.Relaunch = !selfUpdateFlags.noRelaunch
	res, err := update.Apply(ctx, opts)
	if err != nil {
		// jsonOrError surfaces the actionable message for every failure,
		// including the Homebrew-managed refusal (ErrHomebrewManaged) which
		// already tells the user to run `brew upgrade`.
		return jsonOrError(err)
	}
	if res.FromVersion == res.ToVersion {
		if jsonOutput {
			return emitJSON(map[string]string{jsonFieldStatus: "up-to-date", "version": res.ToVersion})
		}
		fmt.Printf("%s Already up to date (%s)\n", success("✓"), value(res.ToVersion))
		return nil
	}
	if jsonOutput {
		return emitJSON(map[string]any{
			jsonFieldStatus: "updated",
			"from":          res.FromVersion,
			"to":            res.ToVersion,
			"relaunched":    res.Relaunched,
		})
	}
	fmt.Printf("%s Updated %s → %s\n", success("✓"), subtle(res.FromVersion), value(res.ToVersion))
	if res.Relaunched {
		fmt.Println(subtle("Relaunching Bladerunner..."))
	}
	return nil
}

// checkStatus maps the boolean to a stable JSON status string.
func checkStatus(available bool) string {
	if available {
		return "update-available"
	}
	return "up-to-date"
}
