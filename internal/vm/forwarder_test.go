//go:build darwin

package vm

import (
	"errors"
	"net"
	"strings"
	"testing"
)

// TestPortForwarderBindConflict verifies the S3 fix: when the host-side listen
// port is already taken, Start returns an actionable message that names the
// port and points at the `br config set` remedy, instead of a bare
// "address already in use".
func TestPortForwarderBindConflict(t *testing.T) {
	// Occupy an ephemeral port so the forwarder's bind is guaranteed to fail.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	defer func() { _ = occupied.Close() }()

	addr := occupied.Addr().String()
	_, port, _ := net.SplitHostPort(addr)

	f := newPortForwarder("ssh", addr, 10022, func(uint32) (net.Conn, error) {
		return nil, errors.New("unused")
	})
	err = f.Start(t.Context())
	if err == nil {
		_ = f.Close()
		t.Fatal("Start over an occupied port returned nil; want error")
	}

	msg := err.Error()
	if !strings.Contains(msg, port) {
		t.Errorf("error %q does not name the conflicting port %q", msg, port)
	}
	if !strings.Contains(msg, "already in use") {
		t.Errorf("error %q missing 'already in use'", msg)
	}
	// "ssh" forwarder must reference its real remedy config key.
	if !strings.Contains(msg, "local-ssh-port") {
		t.Errorf("error %q missing 'br config set local-ssh-port' remedy", msg)
	}
}

func TestConfigKeyForForwarder(t *testing.T) {
	cases := map[string]string{
		"ssh":       "local-ssh-port",
		"incus-api": "local-api-port",
		"other":     "",
	}
	for name, want := range cases {
		if got := configKeyForForwarder(name); got != want {
			t.Errorf("configKeyForForwarder(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestPortFromAddr(t *testing.T) {
	if got := portFromAddr("127.0.0.1:6022"); got != "6022" {
		t.Errorf("portFromAddr = %q, want 6022", got)
	}
	// Non host:port input falls back to the raw string.
	if got := portFromAddr("garbage"); got != "garbage" {
		t.Errorf("portFromAddr fallback = %q, want garbage", got)
	}
}
