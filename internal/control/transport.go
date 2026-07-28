package control

import (
	"net"
	"time"
)

// Transport abstracts the underlying connection mechanism so a Client or a
// Listener can be pointed at a test double. UnixTransport is the only
// implementation in the tree, and the only one the protocol is designed for:
// the socket path is the instance identity and its 0700 directory is the access
// control, neither of which a network transport would carry.
type Transport interface {
	// Listen creates a listener at the given address
	Listen(address string) (net.Listener, error)
	// Dial connects to the given address with a timeout
	Dial(address string, timeout time.Duration) (net.Conn, error)
	// Cleanup performs any necessary cleanup (e.g., removing socket files)
	Cleanup(address string) error
}

// UnixTransport implements Transport using Unix domain sockets.
type UnixTransport struct{}

// Listen creates a Unix socket listener.
func (UnixTransport) Listen(address string) (net.Listener, error) {
	return net.Listen("unix", address)
}

// Dial connects to a Unix socket with timeout.
func (UnixTransport) Dial(address string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("unix", address, timeout)
}

// Cleanup removes the socket file if it exists.
func (UnixTransport) Cleanup(address string) error {
	return removeIfExists(address)
}

// TCP had an implementation here once. It was removed: nothing in the tree or
// its tests ever constructed one, the control protocol is unix-socket-only (the
// socket path IS the instance identity, and its 0700 directory IS the access
// control), and the ADR in issue #29 that proposes publishing a client SDK does
// not carry Transport into the public surface — it moves Client, Message,
// LineFormat and the command constants, and keeps the listener side internal.
// Adding a TCP transport back would put the control plane on a port with no
// authentication in front of it, so it needs a decision, not a revival.

// DefaultTransport is the transport used by default (Unix sockets).
var DefaultTransport Transport = UnixTransport{}
