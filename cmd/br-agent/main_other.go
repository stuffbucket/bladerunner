//go:build !linux

package main

import (
	"fmt"
	"os"
)

// main on non-Linux platforms prints a friendly error. br-agent is only
// meaningful inside a Linux guest VM where AF_VSOCK is available.
func main() {
	fmt.Fprintln(os.Stderr, "br-agent is only supported on linux")
	os.Exit(1)
}
