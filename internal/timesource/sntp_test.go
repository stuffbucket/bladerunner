package timesource

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestResponder_ReplyShape(t *testing.T) {
	r, err := NewResponder("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewResponder: %v", err)
	}
	r.Start()
	defer func() { _ = r.Stop() }()

	conn, err := net.Dial("tcp", r.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	var req [48]byte
	req[0] = 0x23 // LI=0, VN=4, mode=3 (client)
	clientTx := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	copy(req[40:48], clientTx)

	before := time.Now()
	if _, err := conn.Write(req[:]); err != nil {
		t.Fatalf("Write: %v", err)
	}
	var resp [48]byte
	if _, err := io.ReadFull(conn, resp[:]); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	after := time.Now()

	if resp[0]&0x7 != 4 {
		t.Errorf("mode = %d, want 4 (server)", resp[0]&0x7)
	}
	if (resp[0]>>3)&0x7 != 4 {
		t.Errorf("VN not echoed, got %d", (resp[0]>>3)&0x7)
	}
	if resp[1] != 1 {
		t.Errorf("stratum = %d, want 1", resp[1])
	}
	if !bytes.Equal(resp[24:32], clientTx) {
		t.Errorf("origin ts not echoed from client transmit: got %v want %v", resp[24:32], clientTx)
	}
	if !bytes.Equal(resp[12:16], []byte("BLDR")) {
		t.Errorf("refid = %q, want BLDR", resp[12:16])
	}
	// transmit ts (40-47) ~= now: decode seconds, convert from NTP epoch.
	txSecs := int64(binary.BigEndian.Uint32(resp[40:44])) - ntpUnixEpochOffset
	if txSecs < before.Unix()-1 || txSecs > after.Unix()+1 {
		t.Errorf("transmit ts %d not ~= now [%d,%d]", txSecs, before.Unix(), after.Unix())
	}
}

func TestResponder_DeterministicStamp(t *testing.T) {
	r, err := NewResponder("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewResponder: %v", err)
	}
	fixed := time.Date(2026, 6, 1, 12, 0, 0, 500000000, time.UTC)
	r.now = func() time.Time { return fixed }
	r.Start()
	defer func() { _ = r.Stop() }()

	conn, err := net.Dial("tcp", r.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	var req [48]byte
	req[0] = 0x1B // LI=0, VN=3, mode=3 (client)
	if _, err := conn.Write(req[:]); err != nil {
		t.Fatalf("Write: %v", err)
	}
	var resp [48]byte
	if _, err := io.ReadFull(conn, resp[:]); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}

	// VN=3 must be echoed.
	if (resp[0]>>3)&0x7 != 3 {
		t.Errorf("VN not echoed, got %d want 3", (resp[0]>>3)&0x7)
	}
	want := toNTP(fixed)
	for _, off := range []int{16, 32, 40} {
		got := binary.BigEndian.Uint64(resp[off : off+8])
		if got != want {
			t.Errorf("timestamp at offset %d = %#x, want %#x", off, got, want)
		}
	}
}

// TestResponder_PartialRequestNoReply sends fewer than 48 bytes and asserts the
// server does NOT send a reply: serveOne's io.ReadFull must block on the short
// read rather than respond. This exercises the SetDeadline path (sntpConnTimeout).
// If the timeout were mutated to ~0 (e.g. 5*time.Second -> 5/time.Second == 0),
// the deadline would fire immediately and the server would close right away;
// here we assert no 48-byte reply arrives while the (real) 5s deadline is pending.
func TestResponder_PartialRequestNoReply(t *testing.T) {
	r, err := NewResponder("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewResponder: %v", err)
	}
	r.Start()
	defer func() { _ = r.Stop() }()

	conn, err := net.Dial("tcp", r.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// Send only 8 bytes: far short of the required 48. The server's ReadFull
	// must keep blocking (deadline not yet expired), so no reply should arrive.
	if _, err := conn.Write(make([]byte, 8)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Give the client a short read deadline well under sntpConnTimeout (5s).
	// With the real deadline, the server is still blocked => we expect a client
	// timeout, NOT data and NOT a clean EOF.
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	var buf [48]byte
	n, err := conn.Read(buf[:])
	if err == nil {
		t.Fatalf("expected no reply for partial request, got %d bytes", n)
	}
	if n != 0 {
		t.Fatalf("expected 0 bytes for partial request, got %d", n)
	}
	var nerr net.Error
	if !errors.As(err, &nerr) || !nerr.Timeout() {
		// A non-timeout error (e.g. EOF/connection reset) means the server
		// closed the connection early — which is what a near-zero deadline
		// would cause. The real 5s deadline must keep the server blocked.
		t.Fatalf("expected client read timeout (server still blocked on ReadFull), got err=%v", err)
	}
}

func TestToNTP(t *testing.T) {
	// 1970-01-01T00:00:00Z => seconds = ntpUnixEpochOffset, fraction = 0.
	got := toNTP(time.Unix(0, 0).UTC())
	if got>>32 != ntpUnixEpochOffset {
		t.Errorf("seconds = %d, want %d", got>>32, ntpUnixEpochOffset)
	}
	if got&0xFFFFFFFF != 0 {
		t.Errorf("fraction = %d, want 0", got&0xFFFFFFFF)
	}
	// Half-second fraction ~= 1<<31.
	half := toNTP(time.Unix(0, 500000000).UTC())
	frac := half & 0xFFFFFFFF
	if frac < (1<<31)-2 || frac > (1<<31)+2 {
		t.Errorf("half-second fraction = %d, want ~%d", frac, uint64(1)<<31)
	}
}
