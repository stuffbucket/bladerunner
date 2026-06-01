//go:build !unix

package main

import "os/exec"

// detachProcess is a no-op on platforms without POSIX sessions.
func detachProcess(*exec.Cmd) {}
