package main

import (
	"github.com/stuffbucket/bladerunner/internal/control"
	"github.com/stuffbucket/bladerunner/internal/logging"
	"github.com/stuffbucket/bladerunner/internal/ui"
)

// Re-export ui functions for convenience in this package
var (
	title   = ui.Title
	subtle  = ui.Subtle
	success = ui.Success
	warning = ui.Warning
	errorf  = ui.Error
	key     = ui.Key
	value   = ui.Value
	command = ui.Command
)

// sshConfigFromControl retrieves the SSH config path from the running bladerunner
// instance, offering to start the VM first when one is needed (see requireRunningVM).
func sshConfigFromControl() (string, error) {
	client, err := requireRunningVM()
	if err != nil {
		return "", err
	}
	configPath, err := client.GetConfig(control.ConfigKeySSHConfigPath)
	if err != nil {
		// Keep the raw control-socket error out of the terminal; log it instead.
		logging.L().Debug("get ssh config path failed", "err", err)
		return "", errVMNotRunning
	}
	return configPath, nil
}
