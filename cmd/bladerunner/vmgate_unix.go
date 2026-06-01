//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// detachProcess puts the child in its own session so it survives the parent
// command exiting and has no controlling terminal.
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
