package control

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/stuffbucket/bladerunner/internal/logging"
)

// Socket and timeout constants
const (
	SocketName         = "control.sock"
	SocketCheckTimeout = 100 * time.Millisecond

	// Timeouts for client/server operations
	dialTimeout       = 1 * time.Second
	serverRWTimeout   = 5 * time.Second
	clientPingTimeout = 2 * time.Second
	clientCmdTimeout  = 5 * time.Second
)

// Protocol commands
const (
	cmdPing   = "ping"
	cmdStop   = "stop"
	cmdStatus = "status"
)

// Protocol responses
const (
	respOK      = "ok"
	respPong    = "pong"
	respRunning = "running"
	respStopped = "stopped"
	respErrFmt  = "error: %s"
)

// Server listens on a Unix socket for control commands
type Server struct {
	socketPath string
	listener   net.Listener
	stopFunc   func()
	mu         sync.Mutex
	done       chan struct{}
}

// NewServer creates a control server at the given state directory
func NewServer(stateDir string, stopFunc func()) (*Server, error) {
	socketPath := filepath.Join(stateDir, SocketName)

	// Remove socket only if it's not responding (stale)
	if _, err := net.DialTimeout("unix", socketPath, SocketCheckTimeout); err == nil {
		// Socket is responsive, server still running
		return nil, fmt.Errorf("server already running on %s", socketPath)
	}
	// Socket is not responsive or doesn't exist, safe to remove
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove stale socket: %w", err)
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", socketPath, err)
	}

	// Restrict permissions to owner only
	if err := os.Chmod(socketPath, 0o600); err != nil {
		listener.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}

	return &Server{
		socketPath: socketPath,
		listener:   listener,
		stopFunc:   stopFunc,
		done:       make(chan struct{}),
	}, nil
}

// Start begins accepting connections (blocking)
func (s *Server) Start(ctx context.Context) {
	defer close(s.done)

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				logging.L().Warn("control socket accept error", "error", err)
				continue
			}
		}
		go s.handleConnection(conn)
	}
}

func (s *Server) handleConnection(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(serverRWTimeout))

	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		return
	}

	cmd := strings.TrimSpace(line)
	switch cmd {
	case cmdStop:
		_, _ = conn.Write([]byte(respOK + "\n"))
		s.mu.Lock()
		if s.stopFunc != nil {
			s.stopFunc()
		}
		s.mu.Unlock()
	case cmdStatus:
		_, _ = conn.Write([]byte(respRunning + "\n"))
	case cmdPing:
		_, _ = conn.Write([]byte(respPong + "\n"))
	default:
		_, _ = conn.Write([]byte(fmt.Sprintf(respErrFmt, "unknown command") + "\n"))
	}
}

// Close shuts down the control server
func (s *Server) Close() error {
	s.mu.Lock()
	s.stopFunc = nil // prevent stop from being called during shutdown
	s.mu.Unlock()

	var errs []error
	if s.listener != nil {
		if err := s.listener.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close listener: %w", err))
		}
	}
	if err := os.Remove(s.socketPath); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("remove socket: %w", err))
	}
	if len(errs) > 0 {
		return errs[0] // return first error
	}
	return nil
}

// SocketPath returns the path to the control socket
func SocketPath(stateDir string) string {
	return filepath.Join(stateDir, SocketName)
}

// Dialer abstracts connection creation for testing
type Dialer interface {
	Dial(network, address string, timeout time.Duration) (net.Conn, error)
}

// NetDialer is the default Dialer using net.DialTimeout
type NetDialer struct{}

// Dial implements Dialer using net.DialTimeout
func (NetDialer) Dial(network, address string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout(network, address, timeout)
}

// Client sends commands to a running bladerunner instance
type Client struct {
	socketPath string
	dialer     Dialer
}

// NewClient creates a client for the control socket
func NewClient(stateDir string) *Client {
	return &Client{
		socketPath: SocketPath(stateDir),
		dialer:     NetDialer{},
	}
}

// NewClientWithDialer creates a client with a custom dialer for testing
func NewClientWithDialer(stateDir string, dialer Dialer) *Client {
	return &Client{
		socketPath: SocketPath(stateDir),
		dialer:     dialer,
	}
}

// sendCommand sends a command and returns the response
func (c *Client) sendCommand(cmd string, timeout time.Duration) (string, error) {
	conn, err := c.dialer.Dial("unix", c.socketPath, dialTimeout)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return "", fmt.Errorf("set deadline: %w", err)
	}
	if _, err := conn.Write([]byte(cmd + "\n")); err != nil {
		return "", fmt.Errorf("send command: %w", err)
	}

	reader := bufio.NewReader(conn)
	resp, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	return strings.TrimSpace(resp), nil
}

// IsRunning checks if a bladerunner instance is running
func (c *Client) IsRunning() bool {
	resp, err := c.sendCommand(cmdPing, clientPingTimeout)
	if err != nil {
		return false
	}
	return resp == respPong
}

// Stop sends a stop command to the running instance
func (c *Client) Stop() error {
	resp, err := c.sendCommand(cmdStop, clientCmdTimeout)
	if err != nil {
		if isSocketNotAvailable(err) {
			return fmt.Errorf("VM is not running")
		}
		return fmt.Errorf("send stop: %w", err)
	}
	if resp != respOK {
		return fmt.Errorf("unexpected response: %s", resp)
	}
	return nil
}

// Status gets the status of the running instance
func (c *Client) Status() (string, error) {
	resp, err := c.sendCommand(cmdStatus, clientPingTimeout)
	if err != nil {
		if isSocketNotAvailable(err) {
			return respStopped, nil
		}
		return "", fmt.Errorf("get status: %w", err)
	}
	return resp, nil
}

// isSocketNotAvailable returns true if the error indicates the socket doesn't exist
// or the connection was refused (server not running).
func isSocketNotAvailable(err error) bool {
	if err == nil {
		return false
	}
	if os.IsNotExist(err) {
		return true
	}
	errStr := err.Error()
	return strings.Contains(errStr, "no such file") ||
		strings.Contains(errStr, "connection refused")
}
