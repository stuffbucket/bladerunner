package control

import (
	"fmt"
	"net"
	"time"
)

// ClientConfig holds client configuration options.
type ClientConfig struct {
	StateDir  string
	Transport Transport
	Codec     Codec
}

// Client sends commands to a running control server.
type Client struct {
	address   string
	transport Transport
	codec     Codec
}

// NewClient creates a client with default transport and codec.
func NewClient(stateDir string) *Client {
	return NewClientWithConfig(ClientConfig{
		StateDir:  stateDir,
		Transport: DefaultTransport,
		Codec:     DefaultCodec,
	})
}

// NewClientWithConfig creates a client with custom configuration.
func NewClientWithConfig(cfg ClientConfig) *Client {
	if cfg.Transport == nil {
		cfg.Transport = DefaultTransport
	}
	if cfg.Codec == nil {
		cfg.Codec = DefaultCodec
	}
	return &Client{
		address:   SocketPath(cfg.StateDir),
		transport: cfg.Transport,
		codec:     cfg.Codec,
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
		address:   SocketPath(stateDir),
		transport: &dialerAdapter{dialer: dialer},
		codec:     DefaultCodec,
	}
}

// dialerAdapter wraps a Dialer as a Transport for backward compatibility.
type dialerAdapter struct {
	dialer Dialer
}

func (d *dialerAdapter) Listen(address string) (net.Listener, error) {
	return nil, fmt.Errorf("dialerAdapter does not support Listen")
}

func (d *dialerAdapter) Dial(address string, timeout time.Duration) (net.Conn, error) {
	return d.dialer.Dial("unix", address, timeout)
}

func (d *dialerAdapter) Cleanup(address string) error {
	return nil
}

// sendCommand sends a command and returns the response.
func (c *Client) sendCommand(cmd string, timeout time.Duration) (*Message, error) {
	conn, err := c.transport.Dial(c.address, dialTimeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, fmt.Errorf("set deadline: %w", err)
	}

	if err := c.codec.Encode(conn, &Message{Command: cmd}); err != nil {
		return nil, fmt.Errorf("send command: %w", err)
	}

	resp, err := c.codec.Decode(conn)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	return resp, nil
}

// Send sends an arbitrary command and returns the response.
func (c *Client) Send(cmd string) (*Message, error) {
	return c.sendCommand(cmd, clientCmdTimeout)
}

// IsRunning checks if a bladerunner instance is running.
func (c *Client) IsRunning() bool {
	resp, err := c.sendCommand(CmdPing, clientPingTimeout)
	if err != nil {
		return false
	}
	return resp.Response == RespPong
}

// Stop sends a stop command to the running instance.
func (c *Client) Stop() error {
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

// Status gets the status of the running instance.
func (c *Client) Status() (string, error) {
	resp, err := c.sendCommand(CmdStatus, clientPingTimeout)
	if err != nil {
		if isSocketNotAvailable(err) {
			return RespStopped, nil
		}
		return "", fmt.Errorf("get status: %w", err)
	}
	if resp.Error != "" {
		return "", fmt.Errorf("server error: %s", resp.Error)
	}
	return resp.Response, nil
}
