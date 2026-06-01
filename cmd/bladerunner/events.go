package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/incus"
)

var eventsFlags struct {
	types []string
}

var eventsCmd = &cobra.Command{
	Use:   "events",
	Short: "Stream Incus events as JSON",
	Long: `Stream the Incus event firehose as JSON lines. Filter by event type with
--type (repeatable). Valid types: lifecycle, operation, logging, network-acl.`,
	Args: cobra.NoArgs,
	RunE: runEvents,
}

var validEventTypes = map[string]struct{}{
	"lifecycle":   {},
	"operation":   {},
	"logging":     {},
	"network-acl": {},
}

func init() {
	eventsCmd.Flags().StringSliceVar(&eventsFlags.types, "type", nil, "Event type filter (repeatable): lifecycle, operation, logging, network-acl")
}

func runEvents(_ *cobra.Command, _ []string) error {
	if jsonOutput {
		err := fmt.Errorf("--json is not supported for the interactive %q command; use 'runner status --json' or 'runner ls --json' for machine-readable state", "events")
		emitJSONError(err)
		return err
	}

	for _, t := range eventsFlags.types {
		if _, ok := validEventTypes[t]; !ok {
			return fmt.Errorf("invalid --type %q (valid: %s)", t, validEventTypesList())
		}
	}

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

	err = client.MonitorEvents(ctx, incus.EventOptions{Types: eventsFlags.types}, os.Stdout)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func validEventTypesList() string {
	keys := make([]string, 0, len(validEventTypes))
	for k := range validEventTypes {
		keys = append(keys, k)
	}
	return strings.Join(keys, ", ")
}
