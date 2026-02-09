package main

import (
	"fmt"

	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/control"
	"github.com/stuffbucket/bladerunner/internal/logging"
	"github.com/stuffbucket/bladerunner/internal/ui"
)

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

// sshConfigFromControl retrieves the SSH config path from the running bladerunner instance.
func sshConfigFromControl() (string, error) {
	stateDir := config.DefaultStateDir()
	client := control.NewClient(stateDir)

	configPath, err := client.GetConfig(control.ConfigKeySSHConfigPath)
	if err != nil {
		logging.L().Error("VM not running or not configured")
		logging.L().Info("start it with", "command", "br start")
		return "", fmt.Errorf("VM not configured: %w", err)
	}
	return configPath, nil
}
