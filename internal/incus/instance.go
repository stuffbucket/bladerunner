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
	return NewClient(server), nil
}

// NewClient wraps an InstanceServer that is already connected.
//
// Connect calls it after it dials, and it is separate from Connect so that a
// caller holding a connection — one taken from a different code path, or a
// substitute that answers without a server — does not have to open a second.
func NewClient(server incusclient.InstanceServer) *Client {
	return &Client{server: server}
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

// ListInstances returns all instances (containers and VMs) with full state info.
//
// The Incus SDK exposes no context-aware GetInstancesFull, so the request runs
// on its own goroutine and a canceled ctx ABANDONS it rather than interrupting
// it: the caller is released at once, and the goroutine ends whenever the HTTP
// client does. Consulting ctx only before the request — which is what this did
// — made every deadline and every Ctrl-C on top of it a promise it did not
// keep, because the whole wait is inside the call. The result channel is
// buffered so the abandoned goroutine never blocks on a send nobody will read.
func (c *Client) ListInstances(ctx context.Context) ([]api.InstanceFull, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	type listResult struct {
		instances []api.InstanceFull
		err       error
	}
	done := make(chan listResult, 1)
	go func() {
		instances, err := c.server.GetInstancesFull(api.InstanceTypeAny)
		done <- listResult{instances: instances, err: err}
	}()

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("list instances: %w", ctx.Err())
	case res := <-done:
		if res.err != nil {
			return nil, fmt.Errorf("list instances: %w", res.err)
		}
		return res.instances, nil
	}
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
	//
	// The operation is complete, but the websocket relay carrying its output is
	// not necessarily finished, and in the field it has stalled there. A bare
	// `<-dataDone` receive threw away everything WaitContext just honored: no
	// signal, no deadline and no cancellation could reach it, so `br exec` hung
	// with no way out (issue #283). ctx is the way out.
	select {
	case <-dataDone:
	case <-ctx.Done():
		return -1, fmt.Errorf("drain stdio %q: %w", name, ctx.Err())
	}

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
