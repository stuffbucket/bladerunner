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
		status, err := client.GetStatus()
		if err != nil {
			t.Fatalf("GetStatus() error = %v", err)
		}
		if status != "running" {
			t.Errorf("GetStatus() = %q, want %q", status, "running")
		}
	})

	t.Run("stop", func(t *testing.T) {
		if err := client.StopVM(); err != nil {
			t.Fatalf("StopVM() error = %v", err)
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
		status, err := client.GetStatus()
		if err != nil {
			t.Fatalf("GetStatus() error = %v", err)
		}
		if status != "stopped" {
			t.Errorf("GetStatus() = %q, want %q", status, "stopped")
		}
	})

	t.Run("Stop returns error when not running", func(t *testing.T) {
		err := client.StopVM()
		if err == nil {
			t.Error("StopVM() error = nil, want error")
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

	if _, err := conn.Write([]byte("unknown\n")); err != nil {
		t.Fatalf("write error = %v", err)
	}
	buf := make([]byte, 100)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read error = %v", err)
	}
	resp := string(buf[:n])
	if resp != "error: unknown command: unknown\n" {
		t.Errorf("response = %q, want %q", resp, "error: unknown command: unknown\n")
	}
}

// mockConn implements net.Conn for testing
type mockConn struct {
	readData  []byte
	readPos   int
	writeData []byte
	closed    bool
}

func (m *mockConn) Read(b []byte) (n int, err error) {
	if m.readPos >= len(m.readData) {
		return 0, net.ErrClosed
	}
	n = copy(b, m.readData[m.readPos:])
	m.readPos += n
	return n, nil
}

func (m *mockConn) Write(b []byte) (n int, err error) {
	m.writeData = append(m.writeData, b...)
	return len(b), nil
}

func (m *mockConn) Close() error                       { m.closed = true; return nil }
func (m *mockConn) LocalAddr() net.Addr                { return nil }
func (m *mockConn) RemoteAddr() net.Addr               { return nil }
func (m *mockConn) SetDeadline(t time.Time) error      { return nil }
func (m *mockConn) SetReadDeadline(t time.Time) error  { return nil }
func (m *mockConn) SetWriteDeadline(t time.Time) error { return nil }

// mockDialer implements Dialer for testing
type mockDialer struct {
	conn *mockConn
	err  error
}

func (m *mockDialer) Dial(network, address string, timeout time.Duration) (net.Conn, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.conn, nil
}

func TestClientWithMockDialer(t *testing.T) {
	t.Run("IsRunning with mock", func(t *testing.T) {
		conn := &mockConn{readData: []byte("pong\n")}
		dialer := &mockDialer{conn: conn}
		client := NewClientWithDialer("/tmp/test", dialer)

		if !client.IsRunning() {
			t.Error("IsRunning() = false, want true")
		}
		if string(conn.writeData) != "ping\n" {
			t.Errorf("sent = %q, want %q", conn.writeData, "ping\n")
		}
	})

	t.Run("Stop with mock", func(t *testing.T) {
		conn := &mockConn{readData: []byte("ok\n")}
		dialer := &mockDialer{conn: conn}
		client := NewClientWithDialer("/tmp/test", dialer)

		if err := client.StopVM(); err != nil {
			t.Errorf("StopVM() error = %v", err)
		}
		if string(conn.writeData) != "stop\n" {
			t.Errorf("sent = %q, want %q", conn.writeData, "stop\n")
		}
	})

	t.Run("Status with mock", func(t *testing.T) {
		conn := &mockConn{readData: []byte("running\n")}
		dialer := &mockDialer{conn: conn}
		client := NewClientWithDialer("/tmp/test", dialer)

		status, err := client.GetStatus()
		if err != nil {
			t.Errorf("GetStatus() error = %v", err)
		}
		if status != "running" {
			t.Errorf("GetStatus() = %q, want %q", status, "running")
		}
	})
}

func TestNewRequest(t *testing.T) {
	t.Run("simple command", func(t *testing.T) {
		req := NewRequest("ping")
		if req.Command != "ping" {
			t.Errorf("Command = %q, want %q", req.Command, "ping")
		}
		if len(req.Args) != 0 {
			t.Errorf("Args = %v, want empty", req.Args)
		}
	})

	t.Run("command with key=value args", func(t *testing.T) {
		req := NewRequest("config.set key=value timeout=30")
		if req.Command != "config.set" {
			t.Errorf("Command = %q, want %q", req.Command, "config.set")
		}
		if req.Args["key"] != "value" {
			t.Errorf("Args[key] = %q, want %q", req.Args["key"], "value")
		}
		if req.Args["timeout"] != "30" {
			t.Errorf("Args[timeout] = %q, want %q", req.Args["timeout"], "30")
		}
	})

	t.Run("command with positional args", func(t *testing.T) {
		req := NewRequest("echo hello world")
		if req.Args["0"] != "hello" {
			t.Errorf("Args[0] = %q, want %q", req.Args["0"], "hello")
		}
		if req.Args["1"] != "world" {
			t.Errorf("Args[1] = %q, want %q", req.Args["1"], "world")
		}
	})
}

func TestRouter(t *testing.T) {
	t.Run("basic dispatch", func(t *testing.T) {
		router := NewRouter()
		router.HandleFunc("echo", func(_ context.Context, req *Request) *Message {
			return &Message{Response: req.Args["0"]}
		})

		resp := router.Dispatch(context.Background(), NewRequest("echo hello"))
		if resp.Response != "hello" {
			t.Errorf("Response = %q, want %q", resp.Response, "hello")
		}
	})

	t.Run("namespaced commands", func(t *testing.T) {
		configRouter := NewRouter()
		configRouter.HandleFunc("get", func(_ context.Context, req *Request) *Message {
			return &Message{Response: "value-for-" + req.Args["key"]}
		})

		router := NewRouter()
		router.Mount("config", configRouter)

		resp := router.Dispatch(context.Background(), NewRequest("config.get key=name"))
		if resp.Response != "value-for-name" {
			t.Errorf("Response = %q, want %q", resp.Response, "value-for-name")
		}
	})

	t.Run("unknown command", func(t *testing.T) {
		router := NewRouter()
		resp := router.Dispatch(context.Background(), NewRequest("nonexistent"))
		if resp.Error == "" {
			t.Error("expected error for unknown command")
		}
	})

	t.Run("list commands", func(t *testing.T) {
		configRouter := NewRouter()
		configRouter.HandleFunc("get", func(_ context.Context, _ *Request) *Message { return nil })
		configRouter.HandleFunc("set", func(_ context.Context, _ *Request) *Message { return nil })

		router := NewRouter()
		router.HandleFunc("ping", func(_ context.Context, _ *Request) *Message { return nil })
		router.Mount("config", configRouter)

		cmds := router.Commands()
		if len(cmds) != 3 {
			t.Errorf("Commands() len = %d, want 3", len(cmds))
		}
	})
}
