package control

import (
	"net"
	"time"
)

// Transport abstracts the underlying connection mechanism.
// Implementations can provide Unix sockets, TCP, TLS, etc.
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

// TCPTransport implements Transport using TCP sockets.
type TCPTransport struct{}

// Listen creates a TCP listener.
func (TCPTransport) Listen(address string) (net.Listener, error) {
	return net.Listen("tcp", address)
}

// Dial connects to a TCP address with timeout.
func (TCPTransport) Dial(address string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("tcp", address, timeout)
}

// Cleanup is a no-op for TCP (no file to remove).
func (TCPTransport) Cleanup(_ string) error {
	return nil
}

// DefaultTransport is the transport used by default (Unix sockets).
var DefaultTransport Transport = UnixTransport{}
