package main

import "github.com/spf13/cobra"

var menubarBundleOutput string

// menubarCmd runs a small macOS menubar app that mirrors the core CLI: it shows
// the VM status as a colored dot and offers Start / Stop / Open Web UI / Open
// Shell. The actual menubar lives in menubar_darwin.go (macOS-only); other
// platforms get a stub that returns a clear error.
var menubarCmd = &cobra.Command{
	Use:   "menubar",
	Short: "Run a macOS menubar app for bladerunner (status, start, stop, web, shell)",
	Long: `Run a lightweight macOS menubar app for bladerunner.

A status dot in the menubar shows whether the VM is running (green) or stopped
(gray); the menu mirrors the core CLI — Start VM, Stop VM, Open Web UI, and Open
Shell — by invoking the same 'br' commands. The app runs in the foreground;
quit it from its own menu (or Ctrl+C).`,
	RunE: func(_ *cobra.Command, _ []string) error { return runMenubar() },
}

var menubarBundleCmd = &cobra.Command{
	Use:   "bundle",
	Short: "Build a standalone Bladerunner.app (no LaunchAgent) into a directory",
	Long: `Assemble a Bladerunner.app bundle (the br binary + Info.plist + icon) into
--output, without installing a LaunchAgent. Used by the release pipeline to
build the app that is Developer-ID signed, notarized, and packaged as a DMG.`,
	RunE: func(_ *cobra.Command, _ []string) error { return bundleMenubarApp(menubarBundleOutput) },
}

var menubarInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install the menubar as a login agent (a menubar-only app, no dock icon)",
	Long: `Install bladerunner's menubar as a real macOS agent.

Builds a Bladerunner.app bundle (LSUIElement = menubar-only, no dock icon) in
~/Applications and registers a per-user LaunchAgent so it starts at login and
restarts if it crashes. Re-run after upgrading 'br' to refresh the bundle.`,
	RunE: func(_ *cobra.Command, _ []string) error { return installMenubarAgent() },
}

var menubarUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove the menubar login agent and its app bundle",
	RunE:  func(_ *cobra.Command, _ []string) error { return uninstallMenubarAgent() },
}

func init() {
	menubarBundleCmd.Flags().StringVar(&menubarBundleOutput, "output", ".", "Directory to write Bladerunner.app into")
	menubarCmd.AddCommand(menubarInstallCmd, menubarUninstallCmd, menubarBundleCmd)
}
