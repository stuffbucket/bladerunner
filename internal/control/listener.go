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

// Network and timeout constants
const (
	SocketName         = "control.sock"
	SocketCheckTimeout = 100 * time.Millisecond

	dialTimeout       = 1 * time.Second
	listenerRWTimeout = 5 * time.Second
	clientPingTimeout = 2 * time.Second
	clientCmdTimeout  = 5 * time.Second
)

// ListenerConfig holds configuration for a control listener.
type ListenerConfig struct {
	StateDir   string
	Transport  Transport
	WireFormat WireFormat
	Controller Controller
}

// Listener accepts control connections and dispatches commands.
type Listener struct {
	address    string
	transport  Transport
	wireFormat WireFormat
	netListen  net.Listener
	router     *Router
	done       chan struct{}
}

// NewListener creates a control listener with default configuration.
func NewListener(stateDir string, ctrl Controller) (*Listener, error) {
	return NewListenerWithConfig(ListenerConfig{
		StateDir:   stateDir,
		Transport:  DefaultTransport,
		WireFormat: DefaultWireFormat,
		Controller: ctrl,
	})
}

// NewListenerWithConfig creates a control listener with custom configuration.
func NewListenerWithConfig(cfg ListenerConfig) (*Listener, error) {
	if cfg.Transport == nil {
		cfg.Transport = DefaultTransport
	}
	if cfg.WireFormat == nil {
		cfg.WireFormat = DefaultWireFormat
	}

	address := filepath.Join(cfg.StateDir, SocketName)

	// Check if listener is already running
	conn, err := cfg.Transport.Dial(address, SocketCheckTimeout)
	if err == nil {
		conn.Close()
		return nil, fmt.Errorf("listener already running on %s", address)
	}

	// Clean up stale socket
	if err := cfg.Transport.Cleanup(address); err != nil {
		return nil, fmt.Errorf("cleanup stale socket: %w", err)
	}

	netListen, err := cfg.Transport.Listen(address)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", address, err)
	}

	// Restrict permissions for Unix sockets
	if _, ok := cfg.Transport.(UnixTransport); ok {
		if err := os.Chmod(address, 0o600); err != nil {
			netListen.Close()
			return nil, fmt.Errorf("chmod socket: %w", err)
		}
	}

	router := NewRouter()
	if cfg.Controller != nil {
		router.RegisterController(cfg.Controller)
	}

	return &Listener{
		address:    address,
		transport:  cfg.Transport,
		wireFormat: cfg.WireFormat,
		netListen:  netListen,
		router:     router,
		done:       make(chan struct{}),
	}, nil
}

// RegisterCommand adds a custom command handler.
func (l *Listener) RegisterCommand(name string, handler Handler) {
	l.router.Handle(name, handler)
}

// Router returns the underlying router for advanced configuration.
func (l *Listener) Router() *Router {
	return l.router
}

// Start begins accepting connections (blocking).
func (l *Listener) Start(ctx context.Context) {
	defer close(l.done)

	for {
		conn, err := l.netListen.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				logging.L().Warn("control listener accept error", "error", err)
				continue
			}
		}
		go l.handleConnection(conn)
	}
}

func (l *Listener) handleConnection(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(listenerRWTimeout))

	msg, err := l.wireFormat.Decode(conn)
	if err != nil {
		return
	}

	ctx := context.Background()
	req := NewRequest(msg.Command)
	resp := l.router.Dispatch(ctx, req)
	_ = l.wireFormat.Encode(conn, resp)
}

// Close shuts down the control listener.
func (l *Listener) Close() error {
	var errs []error
	if l.netListen != nil {
		if err := l.netListen.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close listener: %w", err))
		}
	}
	if err := l.transport.Cleanup(l.address); err != nil {
		errs = append(errs, fmt.Errorf("cleanup: %w", err))
	}
	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}

// SocketPath returns the socket path for a state directory.
func SocketPath(stateDir string) string {
	return filepath.Join(stateDir, SocketName)
}

// --- Backward compatibility ---

// Server is deprecated, use Listener instead.
type Server = Listener

// ServerConfig is deprecated, use ListenerConfig instead.
type ServerConfig = ListenerConfig

// NewServer creates a listener (deprecated, use NewListener).
func NewServer(stateDir string, stopFunc func()) (*Listener, error) {
	ctrl := NewLocalController(stopFunc)
	return NewListener(stateDir, ctrl)
}

// NewServerWithConfig creates a listener (deprecated, use NewListenerWithConfig).
func NewServerWithConfig(cfg ServerConfig) (*Listener, error) {
	if cfg.Controller == nil {
		return nil, fmt.Errorf("Controller is required")
	}
	return NewListenerWithConfig(cfg)
}

// --- Utility functions ---

// removeIfExists removes a file if it exists.
func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// isSocketNotAvailable returns true if the error indicates the socket is unavailable.
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
