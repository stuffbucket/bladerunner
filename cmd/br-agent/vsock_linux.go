//go:build linux

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// localGuestCID is the well-known AF_VSOCK CID assigned to a guest VM
// (VMADDR_CID_GUEST). Used purely for cosmetic LocalAddr reporting; the
// kernel selects the actual local CID on connect.
const localGuestCID = 3

// vsockConn wraps a vsock file descriptor as a net.Conn so it can be used
// with the generic agent protocol code.
type vsockConn struct {
	*os.File
	localAddr  vsockAddr
	remoteAddr vsockAddr
}

type vsockAddr struct {
	CID  uint32
	Port uint32
}

func (a vsockAddr) Network() string { return "vsock" }
func (a vsockAddr) String() string  { return fmt.Sprintf("%d:%d", a.CID, a.Port) }

func (v *vsockConn) LocalAddr() net.Addr  { return v.localAddr }
func (v *vsockConn) RemoteAddr() net.Addr { return v.remoteAddr }

// dialVsock opens a single AF_VSOCK SOCK_STREAM connection to the host CID
// and port. Caller must close the returned net.Conn.
func dialVsock(cid, port uint32) (net.Conn, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("socket AF_VSOCK: %w", err)
	}
	sa := &unix.SockaddrVM{CID: cid, Port: port}
	if err := unix.Connect(fd, sa); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("connect vsock %d:%d: %w", cid, port, err)
	}
	f := os.NewFile(uintptr(fd), fmt.Sprintf("vsock:%d:%d", cid, port))
	return &vsockConn{
		File:       f,
		localAddr:  vsockAddr{CID: localGuestCID, Port: 0},
		remoteAddr: vsockAddr{CID: cid, Port: port},
	}, nil
}

// dialVsockWithRetry retries dialVsock with backoff until the host listener
// accepts, ctx is canceled, or maxWait elapses.
func dialVsockWithRetry(ctx context.Context, cid, port uint32, maxWait, retryInterval time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(maxWait)
	var lastErr error
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		conn, err := dialVsock(cid, port)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timeout dialing vsock %d:%d after %s: %w", cid, port, maxWait, lastErr)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(retryInterval):
		}
	}
}

// Compile-time guarantee that vsockConn satisfies net.Conn.
var _ net.Conn = (*vsockConn)(nil)
