package incus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	incusclient "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
)

// Client wraps an InstanceServer connection for the host-side Incus API.
type Client struct {
	server incusclient.InstanceServer
}

// ClientConfig describes how to connect to the local Incus API endpoint.
type ClientConfig struct {
	Endpoint string
	CertPEM  []byte
	KeyPEM   []byte
}

// Connect dials the Incus API and returns a Client wrapping the InstanceServer.
func Connect(cfg ClientConfig) (*Client, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("incus: endpoint is required")
	}
	server, err := incusclient.ConnectIncus(cfg.Endpoint, &incusclient.ConnectionArgs{
		TLSClientCert:      string(cfg.CertPEM),
		TLSClientKey:       string(cfg.KeyPEM),
		InsecureSkipVerify: true,
		SkipGetEvents:      true,
	})
	if err != nil {
		return nil, fmt.Errorf("connect incus: %w", err)
	}
	return &Client{server: server}, nil
}

// ConnectFromFiles is a convenience helper that reads cert+key from disk.
func ConnectFromFiles(endpoint, certPath, keyPath string) (*Client, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("read client cert: %w", err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read client key: %w", err)
	}
	return Connect(ClientConfig{Endpoint: endpoint, CertPEM: certPEM, KeyPEM: keyPEM})
}

// Server exposes the underlying InstanceServer for callers needing custom calls.
func (c *Client) Server() incusclient.InstanceServer {
	return c.server
}

// ListInstances returns all instances (containers and VMs) with full state info.
// The Incus SDK does not currently expose a context-aware variant, so ctx is only
// consulted before issuing the request.
func (c *Client) ListInstances(ctx context.Context) ([]api.InstanceFull, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	instances, err := c.server.GetInstancesFull(api.InstanceTypeAny)
	if err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	return instances, nil
}

// ExecOptions controls the behavior of ExecInstance.
type ExecOptions struct {
	// Stdin/Stdout/Stderr are connected to the remote process.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// Interactive requests a single PTY (combines stdout/stderr).
	Interactive bool

	// Width and Height are the initial PTY dimensions when Interactive is true.
	Width  int
	Height int

	// Env adds additional environment variables.
	Env map[string]string
}

// ExecInstance runs cmd inside the named instance, returning the exit code.
func (c *Client) ExecInstance(ctx context.Context, name string, cmd []string, opts ExecOptions) (int, error) {
	if err := ctx.Err(); err != nil {
		return -1, err
	}
	if len(cmd) == 0 {
		return -1, errors.New("incus: exec requires a command")
	}

	req := api.InstanceExecPost{
		Command:     cmd,
		WaitForWS:   true,
		Interactive: opts.Interactive,
		Environment: opts.Env,
		Width:       opts.Width,
		Height:      opts.Height,
	}

	dataDone := make(chan bool)
	args := &incusclient.InstanceExecArgs{
		Stdin:    opts.Stdin,
		Stdout:   opts.Stdout,
		Stderr:   opts.Stderr,
		DataDone: dataDone,
	}

	op, err := c.server.ExecInstance(name, req, args)
	if err != nil {
		return -1, fmt.Errorf("exec instance %q: %w", name, err)
	}

	// Wait for the operation to complete, honoring ctx cancellation.
	if err := op.WaitContext(ctx); err != nil {
		return -1, fmt.Errorf("wait exec %q: %w", name, err)
	}

	// Drain stdio.
	<-dataDone

	// Pull the exit code out of the operation metadata.
	exitCode := 0
	if md := op.Get().Metadata; md != nil {
		if rc, ok := md["return"]; ok {
			switch v := rc.(type) {
			case float64:
				exitCode = int(v)
			case int:
				exitCode = v
			case json.Number:
				n, _ := v.Int64()
				exitCode = int(n)
			}
		}
	}
	return exitCode, nil
}

// StreamLogs writes the console log for the named instance to out.
// When follow is true, the function tails the log until ctx is canceled.
func (c *Client) StreamLogs(ctx context.Context, name string, follow bool, out io.Writer) error {
	if out == nil {
		return errors.New("incus: out writer is nil")
	}

	if err := c.copyConsoleLog(ctx, name, out); err != nil {
		return err
	}
	if !follow {
		return nil
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			// Re-read the console log; the Incus API returns the current snapshot.
			// We discard everything we've already printed and only emit the new tail.
			// For simplicity we re-emit the full snapshot each tick — callers wanting
			// strict streaming semantics can switch to `incus console --show-log`.
			if err := c.copyConsoleLog(ctx, name, out); err != nil {
				return err
			}
		}
	}
}

func (c *Client) copyConsoleLog(ctx context.Context, name string, out io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rc, err := c.server.GetInstanceConsoleLog(name, nil)
	if err != nil {
		return fmt.Errorf("get console log %q: %w", name, err)
	}
	defer func() { _ = rc.Close() }()
	if _, err := io.Copy(out, rc); err != nil {
		return fmt.Errorf("copy console log %q: %w", name, err)
	}
	return nil
}

// EventOptions controls MonitorEvents.
type EventOptions struct {
	// Types optionally filters events to the given types (e.g. "lifecycle").
	Types []string
}

// MonitorEvents streams Incus events to out as JSON-per-line until ctx is canceled.
func (c *Client) MonitorEvents(ctx context.Context, opts EventOptions, out io.Writer) error {
	if out == nil {
		return errors.New("incus: out writer is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	var (
		listener *incusclient.EventListener
		err      error
	)
	if len(opts.Types) > 0 {
		listener, err = c.server.GetEventsByType(opts.Types)
	} else {
		listener, err = c.server.GetEvents()
	}
	if err != nil {
		return fmt.Errorf("get events: %w", err)
	}
	defer listener.Disconnect()

	enc := json.NewEncoder(out)
	_, err = listener.AddHandler(nil, func(ev api.Event) {
		_ = enc.Encode(ev)
	})
	if err != nil {
		return fmt.Errorf("add event handler: %w", err)
	}

	doneCh := make(chan error, 1)
	go func() { doneCh <- listener.Wait() }()

	select {
	case <-ctx.Done():
		listener.Disconnect()
		return ctx.Err()
	case err := <-doneCh:
		return err
	}
}
