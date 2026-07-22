//go:build darwin

// The host-to-guest port forwarder is only wired up by the darwin VM runner
// (runner_darwin.go); on other platforms the VM runner is an unsupported stub,
// so this file is darwin-tagged to keep it out of those builds.
package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/stuffbucket/bladerunner/internal/logging"
)

const (
	forwarderDialRetries    = 30
	forwarderDialRetryDelay = 500 * time.Millisecond
)

type portForwarder struct {
	name       string
	listenAddr string
	guestPort  uint32

	ln     net.Listener
	dialer func(uint32) (net.Conn, error)

	stop chan struct{}
	wg   sync.WaitGroup
}

func newPortForwarder(name, listenAddr string, guestPort uint32, dialer func(uint32) (net.Conn, error)) *portForwarder {
	return &portForwarder{
		name:       name,
		listenAddr: listenAddr,
		guestPort:  guestPort,
		dialer:     dialer,
		stop:       make(chan struct{}),
	}
}

func (f *portForwarder) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", f.listenAddr)
	if err != nil {
		return wrapListenErr(ctx, f.name, f.listenAddr, err)
	}
	f.ln = ln
	logging.L().Info("started port forwarder", "name", f.name, "listen", f.listenAddr, "guest_vsock_port", f.guestPort)

	f.wg.Go(func() {
		for {
			conn, err := f.ln.Accept()
			if err != nil {
				select {
				case <-f.stop:
				default:
					logging.L().Debug("accept error", "name", f.name, "err", err)
				}
				return
			}

			f.wg.Go(func() {
				defer func() { _ = conn.Close() }()

				guestConn, err := f.dialWithRetry()
				if err != nil {
					logging.L().Warn("forward dial failed after retries", "name", f.name, "guest_vsock_port", f.guestPort, "err", err)
					return
				}
				defer func() { _ = guestConn.Close() }()

				proxyBidirectional(conn, guestConn)
			})
		}
	})

	return nil
}

func (f *portForwarder) dialWithRetry() (net.Conn, error) {
	var lastErr error
	for i := range forwarderDialRetries {
		select {
		case <-f.stop:
			return nil, net.ErrClosed
		default:
		}

		conn, err := f.dialer(f.guestPort)
		if err == nil {
			if i > 0 {
				logging.L().Debug("vsock dial succeeded after retries", "name", f.name, "attempts", i+1)
			}
			return conn, nil
		}
		lastErr = err

		if i < forwarderDialRetries-1 {
			time.Sleep(forwarderDialRetryDelay)
		}
	}
	return nil, lastErr
}

func (f *portForwarder) Close() error {
	close(f.stop)
	if f.ln != nil {
		_ = f.ln.Close()
	}
	f.wg.Wait()
	logging.L().Info("stopped port forwarder", "name", f.name, "listen", f.listenAddr)
	return nil
}

func proxyBidirectional(a, b net.Conn) {
	done := make(chan struct{}, 2)

	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		// Signal write completion so the reverse copy sees EOF.
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}

	go cp(a, b)
	go cp(b, a)

	<-done
}

// configKeyForForwarder maps an internal forwarder name to the writable config
// key a user changes to move the conflicting host port (see
// internal/control/control.go). Returns "" for unknown forwarders so the
// caller can fall back to a generic remedy.
func configKeyForForwarder(name string) string {
	switch name {
	case "ssh":
		return "local-ssh-port"
	case "incus-api":
		return "local-api-port"
	default:
		return ""
	}
}

// wrapListenErr turns a bind failure into an actionable message. For the common
// "address already in use" case it names the port, best-effort identifies the
// process holding it, and points at the exact `br config set` remedy. Any other
// error is returned lightly wrapped with the forwarder name for context.
func wrapListenErr(ctx context.Context, name, listenAddr string, err error) error {
	if !errors.Is(err, syscall.EADDRINUSE) {
		return fmt.Errorf("start %s forwarder on %s: %w", name, listenAddr, err)
	}

	port := portFromAddr(listenAddr)
	remedy := "stop it or configure a different port"
	if key := configKeyForForwarder(name); key != "" {
		remedy = fmt.Sprintf("stop it or set a different port (br config set %s <port>)", key)
	}

	if holder := listenerHolder(ctx, port); holder != "" {
		return fmt.Errorf("port %s already in use (by %s) — %s", port, holder, remedy)
	}
	return fmt.Errorf("port %s already in use — %s", port, remedy)
}

// portFromAddr extracts the port from a "host:port" listen address, falling
// back to the raw address if it can't be split.
func portFromAddr(listenAddr string) string {
	if _, p, err := net.SplitHostPort(listenAddr); err == nil {
		return p
	}
	return listenAddr
}

// listenerHolder returns a best-effort "<proc> pid <pid>" description of the
// process currently listening on the given TCP port, via lsof. It returns ""
// (rather than erroring) if lsof is missing, times out, or reports nothing —
// the identification is a nicety, not a requirement.
func listenerHolder(ctx context.Context, port string) string {
	if port == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	// -nP: don't resolve hosts/ports (faster, avoids surprises);
	// -sTCP:LISTEN: only the listening socket, not clients connected to it.
	out, err := exec.CommandContext(ctx, "lsof", "-nP", "-iTCP:"+port, "-sTCP:LISTEN").Output()
	if err != nil {
		return ""
	}

	// lsof output: header line, then one row per fd. Columns are
	// COMMAND PID USER ... — take the first data row's command + pid.
	for line := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] == "COMMAND" {
			continue
		}
		return fmt.Sprintf("%s pid %s", fields[0], fields[1])
	}
	return ""
}
