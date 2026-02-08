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

	"github.com/charmbracelet/log"
)

const (
	SocketName = "control.sock"
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

	// Remove stale socket if exists
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
				log.Warn("control socket accept error", "error", err)
				continue
			}
		}
		go s.handleConnection(conn)
	}
}

func (s *Server) handleConnection(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		return
	}

	cmd := strings.TrimSpace(line)
	switch cmd {
	case "stop":
		conn.Write([]byte("ok\n"))
		s.mu.Lock()
		if s.stopFunc != nil {
			s.stopFunc()
		}
		s.mu.Unlock()
	case "status":
		conn.Write([]byte("running\n"))
	case "ping":
		conn.Write([]byte("pong\n"))
	default:
		conn.Write([]byte("error: unknown command\n"))
	}
}

// Close shuts down the control server
func (s *Server) Close() error {
	s.mu.Lock()
	s.stopFunc = nil // prevent stop from being called during shutdown
	s.mu.Unlock()

	if s.listener != nil {
		s.listener.Close()
	}
	os.Remove(s.socketPath)
	return nil
}

// SocketPath returns the path to the control socket
func SocketPath(stateDir string) string {
	return filepath.Join(stateDir, SocketName)
}

// Client sends commands to a running bladerunner instance
type Client struct {
	socketPath string
}

// NewClient creates a client for the control socket
func NewClient(stateDir string) *Client {
	return &Client{
		socketPath: SocketPath(stateDir),
	}
}

// IsRunning checks if a bladerunner instance is running
func (c *Client) IsRunning() bool {
	conn, err := net.DialTimeout("unix", c.socketPath, time.Second)
	if err != nil {
		return false
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(2 * time.Second))
	conn.Write([]byte("ping\n"))

	reader := bufio.NewReader(conn)
	resp, err := reader.ReadString('\n')
	if err != nil {
		return false
	}
	return strings.TrimSpace(resp) == "pong"
}

// Stop sends a stop command to the running instance
func (c *Client) Stop() error {
	conn, err := net.DialTimeout("unix", c.socketPath, time.Second)
	if err != nil {
		if os.IsNotExist(err) || strings.Contains(err.Error(), "connection refused") {
			return fmt.Errorf("VM is not running")
		}
		return fmt.Errorf("connect to control socket: %w", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("stop\n")); err != nil {
		return fmt.Errorf("send stop command: %w", err)
	}

	reader := bufio.NewReader(conn)
	resp, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if strings.TrimSpace(resp) != "ok" {
		return fmt.Errorf("unexpected response: %s", resp)
	}
	return nil
}

// Status gets the status of the running instance
func (c *Client) Status() (string, error) {
	conn, err := net.DialTimeout("unix", c.socketPath, time.Second)
	if err != nil {
		if os.IsNotExist(err) || strings.Contains(err.Error(), "connection refused") {
			return "stopped", nil
		}
		return "", fmt.Errorf("connect to control socket: %w", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write([]byte("status\n")); err != nil {
		return "", fmt.Errorf("send status command: %w", err)
	}

	reader := bufio.NewReader(conn)
	resp, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	return strings.TrimSpace(resp), nil
}
