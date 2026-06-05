//go:build !darwin

package main

import "errors"

// runMenubar is unavailable off macOS: the menubar relies on the macOS status
// bar (and bladerunner itself only runs on macOS via Virtualization.framework).
func runMenubar() error {
	return errors.New("the bladerunner menubar requires macOS")
}
