//go:build darwin

package vm

import (
	"net"
	"sync"

	"github.com/stuffbucket/bladerunner/internal/logging"
)

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
