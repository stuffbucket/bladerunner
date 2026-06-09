package main

import (
	"errors"
	"os"
	"runtime"
	"strings"
)

func init() {
	// Lock main goroutine to OS thread for macOS GUI (Virtualization framework)
	runtime.LockOSThread()
}

func main() {
	// Launched as Bladerunner.app (double-clicked from /Applications or the signed
	// DMG) there are no CLI args; default to the menubar so the app "just runs".
	// LaunchServices invokes the bundle executable at .../Contents/MacOS/<name>.
	if len(os.Args) == 1 {
		if exe, err := os.Executable(); err == nil && strings.Contains(exe, ".app/Contents/MacOS/") {
			os.Args = append(os.Args, "menubar")
		}
	}
	if err := rootCmd.Execute(); err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			os.Exit(ee.code)
		}
		os.Exit(1)
	}
}
