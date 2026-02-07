package main

import "github.com/stuffbucket/bladerunner/internal/ui"

// Re-export ui functions for convenience in this package
var (
	title   = ui.Title
	subtle  = ui.Subtle
	success = ui.Success
	errorf  = ui.Error
	key     = ui.Key
	value   = ui.Value
	command = ui.Command
)
