//go:build darwin

package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/Code-Hex/vz/v3"
	"github.com/stuffbucket/bladerunner/internal/logging"
)

// AcceptTimeout bounds how long the host waits for the in-guest br-agent to
// dial the vsock listener after the VM is started.
const AcceptTimeout = 4 * time.Minute

// Listener wraps a vsock VirtioSocketListener with an Accept-with-context
// helper used by the VM runner.
type Listener struct {
	port uint32
	ln   net.Listener

	closeOnce sync.Once
}

// NewListener binds a vsock listener on the supplied VirtioSocketDevice at
// the given port. Call this BEFORE the VM is started so the in-guest agent
// can dial as soon as it boots.
func NewListener(device *vz.VirtioSocketDevice, port uint32) (*Listener, error) {
	if device == nil {
		return nil, errors.New("agent: nil socket device")
	}
	if port == 0 {
		return nil, errors.New("agent: zero vsock port")
	}
	ln, err := device.Listen(port)
	if err != nil {
		return nil, fmt.Errorf("agent: listen vsock port %d: %w", port, err)
	}
	logging.L().Info("agent vsock listener bound", "port", port)
	return &Listener{port: port, ln: ln}, nil
}

// Port returns the bound vsock port.
func (l *Listener) Port() uint32 { return l.port }

// Accept waits for the in-guest agent to connect, honoring ctx cancellation
// and the package-level AcceptTimeout. The returned net.Conn is owned by the
// caller (typically passed to RunHandshake).
func (l *Listener) Accept(ctx context.Context) (net.Conn, error) {
	type acceptResult struct {
		conn net.Conn
		err  error
	}
	done := make(chan acceptResult, 1)
	go func() {
		conn, err := l.ln.Accept()
		done <- acceptResult{conn: conn, err: err}
	}()

	timer := time.NewTimer(AcceptTimeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		_ = l.ln.Close()
		return nil, fmt.Errorf("agent: accept canceled: %w", ctx.Err())
	case <-timer.C:
		_ = l.ln.Close()
		return nil, fmt.Errorf("agent: no in-guest connection within %s", AcceptTimeout)
	case r := <-done:
		if r.err != nil {
			return nil, fmt.Errorf("agent: accept: %w", r.err)
		}
		logging.L().Info("agent connection accepted", "remote", r.conn.RemoteAddr().String())
		return r.conn, nil
	}
}

// Close shuts down the underlying vsock listener.
func (l *Listener) Close() error {
	var err error
	l.closeOnce.Do(func() {
		if l.ln != nil {
			err = l.ln.Close()
		}
	})
	return err
}
