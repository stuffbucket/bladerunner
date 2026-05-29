package vm

import (
	"io"
	"net"
	"sync"
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

func (f *portForwarder) Start() error {
	ln, err := net.Listen("tcp", f.listenAddr)
	if err != nil {
		return err
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

// reversePortForwarder accepts vsock connections from the guest and forwards them
// to a host TCP address. This is the mirror image of portForwarder: it makes a
// host-local service (e.g. the bladerunner OIDC provider) reachable from inside
// the VM.
type reversePortForwarder struct {
	name     string
	dialAddr string

	ln net.Listener

	stop chan struct{}
	wg   sync.WaitGroup
}

// newReversePortForwarder constructs a forwarder that accepts on the provided
// listener (typically a VirtioSocketListener bound to a vsock port) and dials
// dialAddr (typically "127.0.0.1:<host-port>") for each accepted connection.
func newReversePortForwarder(name, dialAddr string, ln net.Listener) *reversePortForwarder {
	return &reversePortForwarder{
		name:     name,
		dialAddr: dialAddr,
		ln:       ln,
		stop:     make(chan struct{}),
	}
}

func (f *reversePortForwarder) Start() error {
	logging.L().Info("started reverse port forwarder",
		"name", f.name,
		"vsock_listen", f.ln.Addr().String(),
		"host_dial", f.dialAddr,
	)

	f.wg.Go(func() {
		for {
			conn, err := f.ln.Accept()
			if err != nil {
				select {
				case <-f.stop:
				default:
					logging.L().Debug("reverse accept error", "name", f.name, "err", err)
				}
				return
			}

			f.wg.Go(func() {
				defer func() { _ = conn.Close() }()

				hostConn, derr := net.Dial("tcp", f.dialAddr)
				if derr != nil {
					logging.L().Warn("reverse dial failed", "name", f.name, "addr", f.dialAddr, "err", derr)
					return
				}
				defer func() { _ = hostConn.Close() }()

				proxyBidirectional(conn, hostConn)
			})
		}
	})
	return nil
}

func (f *reversePortForwarder) Close() error {
	close(f.stop)
	if f.ln != nil {
		_ = f.ln.Close()
	}
	f.wg.Wait()
	logging.L().Info("stopped reverse port forwarder", "name", f.name)
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
