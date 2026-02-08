package control

import (
	"context"
	"fmt"
	"net"
	"time"
)

// ClientConfig holds client configuration options.
type ClientConfig struct {
	StateDir   string
	Transport  Transport
	WireFormat WireFormat
}

// Client sends commands to a running control listener.
// It also implements the Controller interface for remote access.
type Client struct {
	address    string
	transport  Transport
	wireFormat WireFormat
}

// NewClient creates a client with default transport and wire format.
func NewClient(stateDir string) *Client {
	return NewClientWithConfig(ClientConfig{
		StateDir:   stateDir,
		Transport:  DefaultTransport,
		WireFormat: DefaultWireFormat,
	})
}

// NewClientWithConfig creates a client with custom configuration.
func NewClientWithConfig(cfg ClientConfig) *Client {
	if cfg.Transport == nil {
		cfg.Transport = DefaultTransport
	}
	if cfg.WireFormat == nil {
		cfg.WireFormat = DefaultWireFormat
	}
	return &Client{
		address:    SocketPath(cfg.StateDir),
		transport:  cfg.Transport,
		wireFormat: cfg.WireFormat,
	}
}

// Dialer abstracts connection creation for testing.
//
// Deprecated: Use Transport interface instead.
type Dialer interface {
	Dial(network, address string, timeout time.Duration) (net.Conn, error)
}

// NetDialer is the default Dialer using net.DialTimeout.
//
// Deprecated: Use Transport interface instead.
type NetDialer struct{}

// Dial implements Dialer using net.DialTimeout.
func (NetDialer) Dial(network, address string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout(network, address, timeout)
}

// NewClientWithDialer creates a client with a custom dialer for testing.
//
// Deprecated: Use NewClientWithConfig with a custom Transport instead.
func NewClientWithDialer(stateDir string, dialer Dialer) *Client {
	return &Client{
		address:    SocketPath(stateDir),
		transport:  &dialerAdapter{dialer: dialer},
		wireFormat: DefaultWireFormat,
	}
}

// dialerAdapter wraps a Dialer as a Transport for backward compatibility.
type dialerAdapter struct {
	dialer Dialer
}

func (d *dialerAdapter) Listen(_ string) (net.Listener, error) {
	return nil, fmt.Errorf("dialerAdapter does not support Listen")
}

func (d *dialerAdapter) Dial(address string, timeout time.Duration) (net.Conn, error) {
	return d.dialer.Dial("unix", address, timeout)
}

func (d *dialerAdapter) Cleanup(_ string) error {
	return nil
}

// sendCommand sends a command and returns the response.
func (c *Client) sendCommand(cmd string, timeout time.Duration) (*Message, error) {
	conn, err := c.transport.Dial(c.address, dialTimeout)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, fmt.Errorf("set deadline: %w", err)
	}

	if err := c.wireFormat.Encode(conn, &Message{Command: cmd}); err != nil {
		return nil, fmt.Errorf("send command: %w", err)
	}

	resp, err := c.wireFormat.Decode(conn)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	return resp, nil
}

// --- Controller interface implementation ---
// Client implements Controller for remote VM control.

// PingContext implements Controller.Ping.
func (c *Client) PingContext(_ context.Context) error {
	resp, err := c.sendCommand(CmdPing, clientPingTimeout)
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("ping failed: %s", resp.Error)
	}
	return nil
}

// StopContext implements Controller.Stop.
func (c *Client) StopContext(_ context.Context) error {
	resp, err := c.sendCommand(CmdStop, clientCmdTimeout)
	if err != nil {
		if isSocketNotAvailable(err) {
			return fmt.Errorf("VM is not running")
		}
		return fmt.Errorf("send stop: %w", err)
	}
	if resp.Error != "" {
		return fmt.Errorf("server error: %s", resp.Error)
	}
	if resp.Response != RespOK {
		return fmt.Errorf("unexpected response: %s", resp.Response)
	}
	return nil
}

// StatusContext implements Controller.Status.
func (c *Client) StatusContext(_ context.Context) (string, error) {
	resp, err := c.sendCommand(CmdStatus, clientPingTimeout)
	if err != nil {
		if isSocketNotAvailable(err) {
			return StatusStopped, nil
		}
		return "", fmt.Errorf("get status: %w", err)
	}
	if resp.Error != "" {
		return "", fmt.Errorf("server error: %s", resp.Error)
	}
	return resp.Response, nil
}

// Controller interface adapter

func (c *Client) Ping(ctx context.Context) error {
	return c.PingContext(ctx)
}

func (c *Client) Stop(ctx context.Context) error {
	return c.StopContext(ctx)
}

func (c *Client) Status(ctx context.Context) (string, error) {
	return c.StatusContext(ctx)
}

// --- Convenience methods (without context) ---

// IsRunning checks if a bladerunner instance is running.
func (c *Client) IsRunning() bool {
	return c.PingContext(context.Background()) == nil
}

// StopVM stops the VM (convenience method without context).
func (c *Client) StopVM() error {
	return c.StopContext(context.Background())
}

// GetStatus returns the VM status (convenience method without context).
func (c *Client) GetStatus() (string, error) {
	return c.StatusContext(context.Background())
}

// Send sends an arbitrary command and returns the response.
func (c *Client) Send(cmd string) (*Message, error) {
	return c.sendCommand(cmd, clientCmdTimeout)
}
