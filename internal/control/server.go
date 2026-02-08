package control

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
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

// Protocol commands (exported for client use)
const (
	CmdPing   = "ping"
	CmdStop   = "stop"
	CmdStatus = "status"
)

// Protocol responses (exported for client use)
const (
	RespOK      = "ok"
	RespPong    = "pong"
	RespRunning = "running"
	RespStopped = "stopped"
)

// ServerConfig holds server configuration options.
type ServerConfig struct {
	StateDir  string
	Transport Transport
	Codec     Codec
	StopFunc  func()
}

// Server listens for control commands.
type Server struct {
	address   string
	transport Transport
	codec     Codec
	listener  net.Listener
	router    *Router
	done      chan struct{}
}

// NewServer creates a control server with default transport and codec.
func NewServer(stateDir string, stopFunc func()) (*Server, error) {
	return NewServerWithConfig(ServerConfig{
		StateDir:  stateDir,
		Transport: DefaultTransport,
		Codec:     DefaultCodec,
		StopFunc:  stopFunc,
	})
}

// NewServerWithConfig creates a control server with custom configuration.
func NewServerWithConfig(cfg ServerConfig) (*Server, error) {
	if cfg.Transport == nil {
		cfg.Transport = DefaultTransport
	}
	if cfg.Codec == nil {
		cfg.Codec = DefaultCodec
	}

	address := filepath.Join(cfg.StateDir, SocketName)

	// Check if server is already running
	conn, err := cfg.Transport.Dial(address, SocketCheckTimeout)
	if err == nil {
		conn.Close()
		return nil, fmt.Errorf("server already running on %s", address)
	}

	// Clean up stale socket
	if err := cfg.Transport.Cleanup(address); err != nil {
		return nil, fmt.Errorf("cleanup stale socket: %w", err)
	}

	listener, err := cfg.Transport.Listen(address)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", address, err)
	}

	// Restrict permissions for Unix sockets
	if _, ok := cfg.Transport.(UnixTransport); ok {
		if err := os.Chmod(address, 0o600); err != nil {
			listener.Close()
			return nil, fmt.Errorf("chmod socket: %w", err)
		}
	}

	s := &Server{
		address:   address,
		transport: cfg.Transport,
		codec:     cfg.Codec,
		listener:  listener,
		router:    NewRouter(),
		done:      make(chan struct{}),
	}

	// Register default commands
	s.router.HandleFunc(CmdPing, func(_ context.Context, _ *Request) *Message {
		return &Message{Response: RespPong}
	})
	s.router.HandleFunc(CmdStatus, func(_ context.Context, _ *Request) *Message {
		return &Message{Response: RespRunning}
	})
	s.router.HandleFunc(CmdStop, func(_ context.Context, _ *Request) *Message {
		if cfg.StopFunc != nil {
			cfg.StopFunc()
		}
		return &Message{Response: RespOK}
	})

	return s, nil
}

// RegisterCommand adds a custom command handler.
func (s *Server) RegisterCommand(name string, handler Handler) {
	s.router.Handle(name, handler)
}

// Start begins accepting connections (blocking).
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

	msg, err := s.codec.Decode(conn)
	if err != nil {
		return
	}

	ctx := context.Background()
	req := NewRequest(msg.Command)
	resp := s.router.Dispatch(ctx, req)
	_ = s.codec.Encode(conn, resp)
}

// Close shuts down the control server.
func (s *Server) Close() error {
	var errs []error
	if s.listener != nil {
		if err := s.listener.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close listener: %w", err))
		}
	}
	if err := s.transport.Cleanup(s.address); err != nil {
		errs = append(errs, fmt.Errorf("cleanup: %w", err))
	}
	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}

// SocketPath returns the path to the control socket.
func SocketPath(stateDir string) string {
	return filepath.Join(stateDir, SocketName)
}

// removeIfExists removes a file if it exists.
func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
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
