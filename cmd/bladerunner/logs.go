package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

var logsFlags struct {
	follow bool
}

var logsCmd = &cobra.Command{
	Use:               "logs <instance>",
	Short:             "Stream console logs from an Incus instance",
	Long:              `Stream the console log of the named Incus instance. Use --follow to tail.`,
	Args:              cobra.ExactArgs(1),
	RunE:              runLogs,
	ValidArgsFunction: instanceNameCompletion,
}

func init() {
	logsCmd.Flags().BoolVarP(&logsFlags.follow, "follow", "f", false, "Follow log output")
}

func runLogs(_ *cobra.Command, args []string) error {
	if err := rejectJSONForInteractive("logs"); err != nil {
		return err
	}

	instance := args[0]

	client, err := connectIncus()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		<-sigCh
		cancel()
	}()

	err = client.StreamLogs(ctx, instance, logsFlags.follow, os.Stdout)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
