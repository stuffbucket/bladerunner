package control

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
	// saveCommandTimeout bounds the server-side CmdSave handling (pause + write
	// the full guest RAM image), which can run for many seconds.
	saveCommandTimeout = 10 * time.Minute
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

	netListen, err := listenReclaiming(cfg.Transport, address)
	if err != nil {
		return nil, err
	}

	// Restrict permissions for Unix sockets
	if _, ok := cfg.Transport.(UnixTransport); ok {
		if err := os.Chmod(address, 0o600); err != nil {
			_ = netListen.Close()
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

// listenReclaiming creates the listener at address, robustly reclaiming a
// socket left behind by a hard-killed process. A `kill -9` never runs the
// listener's cleanup, so a stale unix socket file lingers; the next start then
// gets EADDRINUSE from Listen even though nobody is actually serving.
//
// Strategy: try Listen; on address-in-use, PROBE whether a live server really
// answers on the socket. If it does → genuinely already running (return that
// error). If nothing answers → the socket is stale from a dead process, so
// remove it and retry Listen exactly ONCE. A second failure returns an error
// carrying the exact manual remedy.
//
// The probe (a real dial) plus the single retry is enough to avoid two racing
// starts both reclaiming: whichever wins Listen holds the socket, and the loser
// sees a live answer from the winner (not a stale socket) and backs off.
func listenReclaiming(transport Transport, address string) (net.Listener, error) {
	ln, err := transport.Listen(address)
	if err == nil {
		return ln, nil
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		return nil, fmt.Errorf("listen on %s: %w", address, err)
	}

	// Address in use: is a real server behind it, or a stale socket?
	if conn, derr := transport.Dial(address, SocketCheckTimeout); derr == nil {
		_ = conn.Close()
		return nil, fmt.Errorf("listener already running on %s", address)
	}

	// Nothing answered: treat as stale and reclaim it once.
	if cerr := transport.Cleanup(address); cerr != nil {
		return nil, fmt.Errorf("remove stale socket %s: %w", address, cerr)
	}
	ln, err = transport.Listen(address)
	if err != nil {
		return nil, fmt.Errorf("listen on %s after reclaiming stale socket (remove it manually: rm %s): %w", address, address, err)
	}
	return ln, nil
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
		go l.handleConnection(ctx, conn)
	}
}

func (l *Listener) handleConnection(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(listenerRWTimeout))

	msg, err := l.wireFormat.Decode(conn)
	if err != nil {
		return
	}

	// Reject unsupported future protocol versions
	if msg.Version > ProtocolVersion {
		resp := &Message{
			Version: ProtocolVersion,
			Error:   fmt.Sprintf("unsupported protocol version %d (server supports up to %d)", msg.Version, ProtocolVersion),
		}
		_ = l.wireFormat.Encode(conn, resp)
		return
	}

	req := NewRequest(msg.Command)
	// Saving a VM's RAM state (multi-GB write) and ejecting (a graceful ACPI
	// shutdown that waits for the guest to power off) can both take many seconds;
	// give them a much longer deadline than the default request timeout.
	if req.Command == CmdSave || req.Command == CmdEject {
		_ = conn.SetDeadline(time.Now().Add(saveCommandTimeout))
	}
	resp := l.router.Dispatch(ctx, req)
	resp.Version = ProtocolVersion
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
