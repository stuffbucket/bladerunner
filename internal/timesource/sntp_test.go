package timesource

import (
	"bytes"
	"encoding/binary"
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
