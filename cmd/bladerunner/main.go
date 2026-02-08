package main

import (
	"os"
	"runtime"
)

func init() {
	// Lock main goroutine to OS thread for macOS GUI (Virtualization framework)
	runtime.LockOSThread()
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
