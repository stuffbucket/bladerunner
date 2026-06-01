package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/control"
	"github.com/stuffbucket/bladerunner/internal/incus"
	"github.com/stuffbucket/bladerunner/internal/logging"
)

// connectIncus dials the local Incus API using config from the running control
// listener, offering to start the VM first when needed (see requireRunningVM).
func connectIncus() (*incus.Client, error) {
	ctl, err := requireRunningVM()
	if err != nil {
		return nil, err
	}
	return incusClientFromControl(ctl)
}

// incusClientFromControl builds an Incus client from an already-connected
// control client. It does not prompt, so it is safe to call from shell
// completion.
func incusClientFromControl(ctl *control.Client) (*incus.Client, error) {
	port, err := ctl.GetConfig(control.ConfigKeyLocalAPIPort)
	if err != nil {
		logging.L().Debug("read local-api-port failed", "err", err)
		return nil, errVMNotRunning
	}
	if port == "" {
		return nil, fmt.Errorf("local-api-port not configured")
	}
	endpoint := fmt.Sprintf("https://127.0.0.1:%s", port)

	cfg, err := config.Default("")
	if err != nil {
		return nil, fmt.Errorf("load defaults: %w", err)
	}
	return incus.ConnectFromFiles(endpoint, cfg.ClientCertPath, cfg.ClientKeyPath)
}

// instanceNameCompletion provides shell completion for instance name arguments.
// Falls back to no completion if the VM is not running or the API is unreachable.
func instanceNameCompletion(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	// Completion must never block on a prompt: check silently and bail if the VM
	// is not running.
	ctl := control.NewClient(config.DefaultStateDir())
	if !ctl.IsRunning() {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	client, err := incusClientFromControl(ctl)
	if err != nil {
		return nil, cobra.ShellCompDirectiveError | cobra.ShellCompDirectiveNoFileComp
	}
	instances, err := client.ListInstances(context.Background())
	if err != nil {
		return nil, cobra.ShellCompDirectiveError | cobra.ShellCompDirectiveNoFileComp
	}
	names := make([]string, 0, len(instances))
	for i := range instances {
		names = append(names, instances[i].Name)
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}
