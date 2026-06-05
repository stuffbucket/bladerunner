//go:build !darwin

package main

import "errors"

func installMenubarAgent() error {
	return errors.New("the bladerunner menubar agent requires macOS")
}

func uninstallMenubarAgent() error {
	return errors.New("the bladerunner menubar agent requires macOS")
}
