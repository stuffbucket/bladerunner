package control

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// ClientConfig holds client configuration options.
type ClientConfig struct {
	StateDir   string
	Transport  Transport
	WireFormat WireFormat
}

// Client sends commands to a running control listener.
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

	if err := c.wireFormat.Encode(conn, &Message{Version: ProtocolVersion, Command: cmd}); err != nil {
		return nil, fmt.Errorf("send command: %w", err)
	}

	resp, err := c.wireFormat.Decode(conn)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.Version > ProtocolVersion {
		return nil, fmt.Errorf("server protocol version %d is newer than client version %d; upgrade bladerunner", resp.Version, ProtocolVersion)
	}

	return resp, nil
}

// --- Context-aware command primitives ---
// The convenience methods below (IsRunning/StopVM/GetStatus) build on these.

// PingContext sends a ping and reports whether the server responded.
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

// StopContext sends a stop command to the running server.
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

// StatusContext queries the running server for its VM status.
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

// ServerVersion returns the running server's build version string, used to
// detect that a newer client binary should take over the server (runner upgrade).
func (c *Client) ServerVersion() (string, error) {
	resp, err := c.sendCommand(CmdServerVersion, clientCmdTimeout)
	if err != nil {
		return "", err
	}
	if resp.Error != "" {
		return "", fmt.Errorf("server error: %s", resp.Error)
	}
	return resp.Response, nil
}

// SaveState asks the server to pause the guest and write its machine state to
// the server's default saved-state path, returning that path. When keepPaused
// is true the guest is left paused (for an upgrade handoff); otherwise it is
// resumed afterward (a live snapshot).
func (c *Client) SaveState(keepPaused bool) (string, error) {
	cmd := CmdSave
	if keepPaused {
		cmd = BuildCommand(CmdSave, SaveModePause)
	}
	resp, err := c.sendCommand(cmd, saveCommandTimeout)
	if err != nil {
		return "", err
	}
	if resp.Error != "" {
		return "", fmt.Errorf("save error: %s", resp.Error)
	}
	return resp.Response, nil
}

// Eject asks the running server to gracefully ACPI-shut-down the guest (waiting
// up to timeoutSeconds for it to power off, then forcing the stop) and exit,
// releasing and detaching any attached cartridge. The server initiates its own
// shutdown after replying, so the caller should wait for the control socket to
// disappear (see waitForSocketGone) rather than additionally sending a stop.
func (c *Client) Eject(force bool, timeoutSeconds int) error {
	args := []string{strconv.Itoa(timeoutSeconds)}
	if force {
		args = append(args, EjectModeForce)
	}
	resp, err := c.sendCommand(BuildCommand(CmdEject, args...), saveCommandTimeout)
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("eject error: %s", resp.Error)
	}
	return nil
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

// GetConfig retrieves a config value from the running instance by key.
func (c *Client) GetConfig(key string) (string, error) {
	resp, err := c.sendCommand(BuildCommand(CmdConfigGet, key), clientCmdTimeout)
	if err != nil {
		return "", fmt.Errorf("get config %s: %w", key, err)
	}
	if resp.Error != "" {
		return "", fmt.Errorf("config error: %s", resp.Error)
	}
	return resp.Response, nil
}

// SetConfig sets a config value on the running instance by key.
func (c *Client) SetConfig(key, value string) error {
	resp, err := c.sendCommand(BuildCommand(CmdConfigSet, key, value), clientCmdTimeout)
	if err != nil {
		return fmt.Errorf("set config %s: %w", key, err)
	}
	if resp.Error != "" {
		return fmt.Errorf("config error: %s", resp.Error)
	}
	return nil
}

// GetConfigKeys retrieves a list of all available config keys.
func (c *Client) GetConfigKeys() ([]string, error) {
	resp, err := c.sendCommand(CmdConfigKeys, clientCmdTimeout)
	if err != nil {
		return nil, fmt.Errorf("get config keys: %w", err)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("config error: %s", resp.Error)
	}
	keys := strings.Fields(resp.Response)
	return keys, nil
}

// Send sends an arbitrary command and returns the response.
func (c *Client) Send(cmd string) (*Message, error) {
	return c.sendCommand(cmd, clientCmdTimeout)
}
