package main

import (
	"errors"
	"os"
	"runtime"
)

func init() {
	// Lock main goroutine to OS thread for macOS GUI (Virtualization framework)
	runtime.LockOSThread()
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			os.Exit(ee.code)
		}
		os.Exit(1)
	}
}
