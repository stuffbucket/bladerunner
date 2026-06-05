package main

import "github.com/spf13/cobra"

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
Shell — by invoking the same 'runner' commands. The app runs in the foreground;
quit it from its own menu (or Ctrl+C).`,
	RunE: func(_ *cobra.Command, _ []string) error { return runMenubar() },
}
