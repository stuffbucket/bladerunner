package control

import (
	"context"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestServerAndClient(t *testing.T) {
	tmpDir := t.TempDir()

	var stopCalled atomic.Bool
	stopFunc := func() {
		stopCalled.Store(true)
	}

	server, err := NewServer(tmpDir, stopFunc)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go server.Start(ctx)

	// Give the server time to start
	time.Sleep(50 * time.Millisecond)

	client := NewClient(tmpDir)

	t.Run("ping", func(t *testing.T) {
		if !client.IsRunning() {
			t.Error("IsRunning() = false, want true")
		}
	})

	t.Run("status", func(t *testing.T) {
		status, err := client.Status()
		if err != nil {
			t.Fatalf("Status() error = %v", err)
		}
		if status != "running" {
			t.Errorf("Status() = %q, want %q", status, "running")
		}
	})

	t.Run("stop", func(t *testing.T) {
		if err := client.Stop(); err != nil {
			t.Fatalf("Stop() error = %v", err)
		}
		if !stopCalled.Load() {
			t.Error("stopFunc was not called")
		}
	})
}

func TestClientNotRunning(t *testing.T) {
	tmpDir := t.TempDir()
	client := NewClient(tmpDir)

	t.Run("IsRunning returns false when not running", func(t *testing.T) {
		if client.IsRunning() {
			t.Error("IsRunning() = true, want false")
		}
	})

	t.Run("Status returns stopped when not running", func(t *testing.T) {
		status, err := client.Status()
		if err != nil {
			t.Fatalf("Status() error = %v", err)
		}
		if status != "stopped" {
			t.Errorf("Status() = %q, want %q", status, "stopped")
		}
	})

	t.Run("Stop returns error when not running", func(t *testing.T) {
		err := client.Stop()
		if err == nil {
			t.Error("Stop() error = nil, want error")
		}
	})
}

func TestSocketPath(t *testing.T) {
	stateDir := "/test/state"
	expected := filepath.Join(stateDir, SocketName)
	got := SocketPath(stateDir)
	if got != expected {
		t.Errorf("SocketPath() = %q, want %q", got, expected)
	}
}

func TestServerClose(t *testing.T) {
	tmpDir := t.TempDir()

	server, err := NewServer(tmpDir, func() {})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	// Close should not error
	if err := server.Close(); err != nil {
		t.Errorf("Close() error = %v", err)
	}

	// Socket should be removed after close
	client := NewClient(tmpDir)
	if client.IsRunning() {
		t.Error("IsRunning() = true after Close(), want false")
	}
}

func TestUnknownCommand(t *testing.T) {
	tmpDir := t.TempDir()

	server, err := NewServer(tmpDir, func() {})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go server.Start(ctx)
	time.Sleep(50 * time.Millisecond)

	// Send unknown command directly
	socketPath := SocketPath(tmpDir)
	conn, err := net.DialTimeout("unix", socketPath, time.Second)
	if err != nil {
		t.Fatalf("dial error = %v", err)
	}
	defer conn.Close()

	conn.Write([]byte("unknown\n"))
	buf := make([]byte, 100)
	n, _ := conn.Read(buf)
	resp := string(buf[:n])
	if resp != "error: unknown command\n" {
		t.Errorf("response = %q, want %q", resp, "error: unknown command\n")
	}
}
