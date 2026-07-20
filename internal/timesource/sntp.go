// Package timesource serves the host clock to the guest as a stratum-1 SNTP
// source over vsock. It is the host end of the pseudo-NTP path:
//
//	guest chrony (UDP 123) -> bladerunner-vsock-relay@ntp (socat UDP->vsock)
//	-> host vsock reverse forwarder -> this Responder (127.0.0.1:LocalNTPPort)
//
// The guest coheres to the HOST clock (not UTC) and works fully OFFLINE: vsock
// needs no IP network. The responder is portable (stdlib only, no OS-specific
// calls) so it compiles cleanly on darwin and GOOS=linux.
package timesource

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"time"

	"github.com/stuffbucket/bladerunner/internal/logging"
)

const (
	// ntpUnixEpochOffset is the seconds between the NTP epoch (1900-01-01) and
	// the Unix epoch (1970-01-01).
	ntpUnixEpochOffset = 2208988800
	// ntpModeMask masks the low 3 mode/version bits of an NTP byte0 field.
	ntpModeMask = 0x7
	// nanosPerSecond is used to scale a fractional second into NTP 32-bit form.
	nanosPerSecond = 1e9
	// sntpConnTimeout bounds a single read-request/write-reply exchange so a
	// stalled peer cannot pin a goroutine + fd.
	sntpConnTimeout = 5 * time.Second
)

// Responder is a stream SNTP server: per accepted connection it reads exactly
// one 48-byte NTP client packet and writes one 48-byte stratum-1 server reply
// stamped from the host clock, then closes. It is the dial target of the host
// vsock NTP reverse forwarder (which relays the 48 bytes in / 48 bytes out from
// the guest socat UDP bridge). Portable: no OS-specific calls.
type Responder struct {
	ln   net.Listener
	stop chan struct{}
	wg   sync.WaitGroup
	now  func() time.Time // injectable for tests; defaults to time.Now
}

// NewResponder binds a TCP listener on addr (e.g. "127.0.0.1:<LocalNTPPort>").
func NewResponder(addr string) (*Responder, error) {
	lc := &net.ListenConfig{}
	ln, err := lc.Listen(context.Background(), "tcp", addr)
	if err != nil {
		return nil, err
	}
	return &Responder{
		ln:   ln,
		stop: make(chan struct{}),
		now:  time.Now,
	}, nil
}

// Start spawns the accept loop. Returns immediately.
func (r *Responder) Start() {
	logging.L().Info("started sntp responder", "addr", r.ln.Addr().String())
	r.wg.Go(func() {
		for {
			conn, err := r.ln.Accept()
			if err != nil {
				select {
				case <-r.stop:
				default:
					logging.L().Debug("sntp accept error", "err", err)
				}
				return
			}
			r.wg.Go(func() {
				defer func() { _ = conn.Close() }()
				if err := r.serveOne(conn); err != nil {
					logging.L().Debug("sntp serve error", "err", err)
				}
			})
		}
	})
}

// Stop closes the listener and waits for in-flight connections.
func (r *Responder) Stop() error {
	close(r.stop)
	err := r.ln.Close()
	r.wg.Wait()
	logging.L().Info("stopped sntp responder")
	return err
}

// Addr returns the bound listener address (useful for tests with port 0).
func (r *Responder) Addr() net.Addr {
	return r.ln.Addr()
}

// serveOne reads exactly 48 bytes, writes exactly 48 bytes, returns (caller
// closes the connection). The Close after one reply is load-bearing: socat's
// per-datagram child needs EOF to flush the UDP response back to chrony. Do NOT
// loop reading multiple requests on one connection.
func (r *Responder) serveOne(conn net.Conn) error {
	// Bound the exchange so a peer that connects and sends fewer than 48 bytes
	// without closing cannot pin a goroutine + fd indefinitely.
	_ = conn.SetDeadline(time.Now().Add(sntpConnTimeout))
	var req [48]byte
	if _, err := io.ReadFull(conn, req[:]); err != nil {
		return err // short/partial read => drop this datagram; chrony retries next poll
	}
	var resp [48]byte
	buildReply(&resp, &req, r.now())
	_, err := conn.Write(resp[:])
	return err
}

// buildReply fills resp with a stratum-1 server reply for the client request
// req, stamped at now. RFC 5905 48-byte layout, all fields big-endian.
func buildReply(resp *[48]byte, req *[48]byte, now time.Time) {
	// byte0: LI(2)|VN(3)|Mode(3). LI=0. Mode=4 (server). VN ECHOED from client.
	vn := (req[0] >> 3) & ntpModeMask
	resp[0] = (vn << 3) | 4 // LI=0, echo VN, mode=4 (server)
	resp[1] = 1             // stratum = 1 (primary)
	resp[2] = req[2]        // echo client poll (sane: 4..6)
	resp[3] = 0xEC          // precision = -20 (~1 microsecond), int8(-20)
	// bytes4-7 root delay = 0, bytes8-11 root dispersion = 0 (already zeroed)
	copy(resp[12:16], "BLDR") // refid: 4 printable ASCII for stratum-1

	ref := toNTP(now)
	binary.BigEndian.PutUint64(resp[16:24], ref)        // reference timestamp = host now
	copy(resp[24:32], req[40:48])                       // ORIGIN = client's transmit ts (echo verbatim)
	binary.BigEndian.PutUint64(resp[32:40], toNTP(now)) // receive timestamp = host now
	binary.BigEndian.PutUint64(resp[40:48], toNTP(now)) // transmit timestamp = host now
}

// toNTP converts a time.Time to a 64-bit NTP timestamp (32.32 fixed point,
// epoch 1900-01-01).
func toNTP(t time.Time) uint64 {
	secs := uint64(t.Unix() + ntpUnixEpochOffset)
	frac := (uint64(t.Nanosecond()) << 32) / nanosPerSecond
	return secs<<32 | frac
}
