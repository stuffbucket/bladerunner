package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/control"
	"github.com/stuffbucket/bladerunner/internal/incus"
)

// connectIncus dials the local Incus API using config from the running control listener.
// It returns an error if the VM is not running.
func connectIncus() (*incus.Client, error) {
	stateDir := config.DefaultStateDir()
	ctl := control.NewClient(stateDir)
	if !ctl.IsRunning() {
		return nil, fmt.Errorf("VM is not running; start it with: br start")
	}

	port, err := ctl.GetConfig(control.ConfigKeyLocalAPIPort)
	if err != nil {
		return nil, fmt.Errorf("read local-api-port: %w", err)
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
	client, err := connectIncus()
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
