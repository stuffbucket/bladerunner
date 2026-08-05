package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/control"
	"github.com/stuffbucket/bladerunner/internal/incus"
	"github.com/stuffbucket/bladerunner/internal/logging"
)

// incusClientFromControl builds an Incus client from an already-connected
// control client. It does not prompt, so it is safe to call from shell
// completion.
//
// target MUST be the instance the control client was built from: the API port
// is read live from that instance, but the client certificate is material on
// disk, and each instance keeps its own client.crt/client.key beside its
// control socket. Taking the port from one instance and the identity from
// another (which is what reading config.Default("") did) fails authentication
// against every non-default instance.
func incusClientFromControl(ctl *control.Client, target resolvedInstance) (*incus.Client, error) {
	port, err := ctl.GetConfig(control.ConfigKeyLocalAPIPort)
	if err != nil {
		logging.L().Debug("read local-api-port failed", "instance", target.instanceName(), "err", err)
		return nil, notRunningError(target)
	}
	if port == "" {
		return nil, errors.New("local-api-port not configured")
	}
	endpoint := fmt.Sprintf("https://127.0.0.1:%s", port)

	cfg, err := config.Default(target.StateDir)
	if err != nil {
		return nil, fmt.Errorf("load defaults: %w", err)
	}
	return incus.ConnectFromFiles(endpoint, cfg.ClientCertPath, cfg.ClientKeyPath)
}

// completionBudget bounds the Incus call behind shell completion.
//
// Completion gets a DEADLINE where `br exec` and `br ls` get a signal, because
// the shell owns the keyboard while it is completing: there is no Ctrl-C to
// press into a call that never answers, so the only bound that can exist here
// is one the process sets itself. An unreachable API must cost the user a
// pause, not their shell.
const completionBudget = 3 * time.Second

// instanceNameCompletion provides shell completion for instance name arguments.
// Falls back to no completion if the VM is not running or the API is unreachable.
func instanceNameCompletion(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	// Completion must never block on a prompt, so it resolves the selected
	// instance and probes it silently rather than going through
	// requireRunningTarget, which offers to start the default VM.
	target, err := resolveInstanceTarget()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	ctl := control.NewClient(target.StateDir)
	if !ctl.IsRunning() {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	client, err := incusClientFromControl(ctl, target)
	if err != nil {
		return nil, cobra.ShellCompDirectiveError | cobra.ShellCompDirectiveNoFileComp
	}
	ctx, cancel := context.WithTimeout(context.Background(), completionBudget)
	defer cancel()
	instances, err := client.ListInstances(ctx)
	if err != nil {
		return nil, cobra.ShellCompDirectiveError | cobra.ShellCompDirectiveNoFileComp
	}
	names := make([]string, 0, len(instances))
	for i := range instances {
		names = append(names, instances[i].Name)
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}
