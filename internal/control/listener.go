package control

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/stuffbucket/bladerunner/internal/logging"
)

// Network and timeout constants
const (
	SocketName         = "control.sock"
	SocketCheckTimeout = 100 * time.Millisecond

	// LockName is the ownership claim taken next to the control socket before
	// the dial/unlink/bind dance in NewListenerWithConfig. See startLock.
	LockName = "control.lock"

	dialTimeout       = 1 * time.Second
	listenerRWTimeout = 5 * time.Second
	clientPingTimeout = 2 * time.Second
	clientCmdTimeout  = 5 * time.Second
	// saveCommandTimeout bounds the server-side CmdSave handling (pause + write
	// the full guest RAM image), which can run for many seconds.
	saveCommandTimeout = 10 * time.Minute

	// lockAttempts bounds the stale-lock reclaim loop: one attempt to create
	// the lock, and one more after reclaiming a lock whose holder is gone.
	lockAttempts = 2
)

// ErrInstanceLocked reports that another live process already claims this
// instance's state directory. It is returned instead of unlinking that
// process's control socket out from under it.
var ErrInstanceLocked = errors.New("another bladerunner process holds this instance")

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
	lock       *startLock
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
//
// Startup is serialized by an O_EXCL lock file next to the socket (see
// startLock). Without it the dial-then-unlink-then-bind sequence below is a
// TOCTOU hole: two starts racing in the same state directory can EACH fail the
// dial (neither is serving yet) and the second then unlinks the first's LIVE
// socket, leaving a VM nobody can reach. The lock closes that window; the bound
// socket remains the authoritative ownership token.
func NewListenerWithConfig(cfg ListenerConfig) (*Listener, error) {
	if cfg.Transport == nil {
		cfg.Transport = DefaultTransport
	}
	if cfg.WireFormat == nil {
		cfg.WireFormat = DefaultWireFormat
	}

	address := SocketPath(cfg.StateDir)

	lock, err := acquireStartLock(cfg.StateDir)
	if err != nil {
		return nil, err
	}

	listener, err := bindListener(cfg, address)
	if err != nil {
		lock.release()
		return nil, err
	}
	listener.lock = lock
	return listener, nil
}

// bindListener performs the dial/cleanup/bind sequence under an already-held
// start lock and assembles the Listener.
func bindListener(cfg ListenerConfig, address string) (*Listener, error) {
	// Check if listener is already running
	conn, err := cfg.Transport.Dial(address, SocketCheckTimeout)
	if err == nil {
		_ = conn.Close()
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

// startLock is a best-effort, crash-tolerant claim on a state directory,
// recorded as the holder's PID in <stateDir>/control.lock.
//
// The file is created with O_CREAT|O_EXCL, so exactly one process wins the
// create. A leftover lock from a holder that crashed is not fatal: the recorded
// PID is probed for liveness (kill(pid, 0)) and a dead owner's lock is
// reclaimed, so a crash never permanently wedges a state dir. A live owner's
// lock is honored — that is the case the old dial-then-unlink code got wrong.
//
// It is deliberately advisory and deliberately degradable: a state directory
// that cannot hold the file at all (missing, read-only, an exotic filesystem)
// falls back to the previous behavior rather than refusing to start a VM.
type startLock struct {
	path string
	pid  int
}

// acquireStartLock claims dir for this process. A *startLock with no path is
// the "unlocked" claim: no file was taken and the caller proceeds unguarded.
// Every method tolerates both that and a nil receiver.
func acquireStartLock(dir string) (*startLock, error) {
	if dir == "" {
		return unlockedStartLock, nil // no directory to anchor a lock file to
	}
	path := LockPath(dir)
	pid := os.Getpid()

	for range lockAttempts {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			return claimStartLock(f, path, pid)
		}
		if !errors.Is(err, fs.ErrExist) {
			// No lock is better than no VM.
			logging.L().Debug("control lock unavailable; falling back to the socket probe",
				"path", path, "error", err)
			return unlockedStartLock, nil
		}
		if owner, readErr := readLockPID(path); readErr == nil && owner != pid && processAlive(owner) {
			return nil, fmt.Errorf("%w: pid %d holds %s", ErrInstanceLocked, owner, path)
		}
		// The recorded holder is gone (or the record is unreadable): reclaim
		// the lock and try once more.
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("reclaim stale control lock %s: %w", path, err)
		}
	}
	return nil, fmt.Errorf("%w: %s is contended", ErrInstanceLocked, path)
}

// unlockedStartLock is the claim returned when no lock file could be — or
// needed to be — taken. Releasing it does nothing.
var unlockedStartLock = &startLock{}

// claimStartLock records pid in the freshly created lock file and verifies the
// record survived. The read-back catches the one residual race the O_EXCL
// create cannot: two processes reclaiming the SAME stale lock can both remove
// it, and the loser's remove can delete the winner's fresh file.
func claimStartLock(f *os.File, path string, pid int) (*startLock, error) {
	_, writeErr := fmt.Fprintf(f, "%d\n", pid)
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("write control lock %s: %w", path, err)
	}
	owner, err := readLockPID(path)
	if err != nil || owner != pid {
		return nil, fmt.Errorf("%w: %s was taken by pid %d", ErrInstanceLocked, path, owner)
	}
	return &startLock{path: path, pid: pid}, nil
}

// release drops the claim. It only removes a lock file that still records this
// process, so releasing after another process legitimately reclaimed a lock we
// had already lost cannot delete that process's claim.
func (l *startLock) release() {
	if l == nil || l.path == "" {
		return
	}
	if owner, err := readLockPID(l.path); err == nil && owner != l.pid {
		return
	}
	_ = os.Remove(l.path)
}

// readLockPID reads the PID recorded in a lock file.
func readLockPID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("parse control lock %s: %w", path, err)
	}
	if pid <= 0 {
		return 0, fmt.Errorf("parse control lock %s: pid %d is not a process", path, pid)
	}
	return pid, nil
}

// processAlive reports whether pid names a live process, using the signal-0
// probe: kill(pid, 0) performs the existence and permission checks without
// delivering a signal. EPERM counts as alive — the process exists, it just
// belongs to another user.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EPERM)
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

// Close shuts down the control listener and releases its start lock.
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
	// The lock goes last: it must outlive the socket it guards, so a start
	// racing this shutdown never sees an unlocked directory with a live socket.
	l.lock.release()
	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}

// LockPath returns the start-lock path for a state directory. It is the single
// definition of that path: acquireStartLock builds its lock file through this
// function rather than re-joining LockName, so an external caller reasoning
// about the lock (a diagnostic, a cleanup) can never disagree with the code
// that takes it.
func LockPath(stateDir string) string {
	return filepath.Join(stateDir, LockName)
}

// SocketPath returns the control socket path for a state directory. Like
// LockPath it is the one source of truth: NewListenerWithConfig binds this
// exact path and NewClient dials it.
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
