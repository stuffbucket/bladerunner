package control

import (
	"context"
	"net"
	"os"
	"testing"
	"time"
)

// TestNewListenerReclaimsStaleSocket verifies the S7 fix: a socket file left
// behind by a hard-killed process (no live server) is transparently reclaimed
// instead of forcing the user to `rm` it manually.
func TestNewListenerReclaimsStaleSocket(t *testing.T) {
	// Short path: unix sockets have a ~104 char limit.
	tmpDir, err := os.MkdirTemp("/tmp", "ctrl-stale-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	socketPath := SocketPath(tmpDir)

	// Simulate a stale socket: bind a listener, then close it WITHOUT removing
	// the file (mimicking kill -9, which never runs Cleanup).
	stale, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("seed stale socket: %v", err)
	}
	// Close the underlying fd but leave the socket file on disk.
	if ul, ok := stale.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	_ = stale.Close()

	if _, err := os.Stat(socketPath); err != nil {
		t.Fatalf("expected stale socket file to remain: %v", err)
	}

	// New should reclaim the stale socket rather than error.
	l, err := NewServer(tmpDir, func() {})
	if err != nil {
		t.Fatalf("NewServer over stale socket: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
}

// TestNewListenerRejectsLiveListener verifies that when a live server is really
// listening, a second start does NOT clobber it and returns "already running".
func TestNewListenerRejectsLiveListener(t *testing.T) {
	tmpDir, err := os.MkdirTemp("/tmp", "ctrl-live-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	first, err := NewServer(tmpDir, func() {})
	if err != nil {
		t.Fatalf("first NewServer: %v", err)
	}
	defer func() { _ = first.Close() }()

	// The first listener must be actively accepting for the probe to answer.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go first.Start(ctx)

	// Poll until the live probe succeeds, then confirm a second start refuses.
	client := NewClient(tmpDir)
	running := false
	for i := 0; i < 50; i++ {
		if client.IsRunning() {
			running = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !running {
		t.Fatal("first listener never became reachable")
	}

	if _, err := NewServer(tmpDir, func() {}); err == nil {
		t.Fatal("second NewServer succeeded over a live listener; want error")
	}
}
