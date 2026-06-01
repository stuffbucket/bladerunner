//go:build !darwin

package main

import (
	"fmt"
	"runtime"
)

func installTrustedCert(_ string, _ bool) error {
	return fmt.Errorf("'br web trust' is only implemented on macOS (this host is %s); import the Incus server cert into your OS/browser trust store manually", runtime.GOOS)
}

func removeTrustedCert(_ bool) error {
	return fmt.Errorf("'br web untrust' is only implemented on macOS (this host is %s)", runtime.GOOS)
}
