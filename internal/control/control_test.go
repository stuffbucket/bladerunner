package control

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
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
		if status != StatusRunning {
			t.Errorf("GetStatus() = %q, want %q", status, StatusRunning)
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
		if status != StatusStopped {
			t.Errorf("GetStatus() = %q, want %q", status, StatusStopped)
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
func (m *mockConn) SetDeadline(_ time.Time) error      { return nil }
func (m *mockConn) SetReadDeadline(_ time.Time) error  { return nil }
func (m *mockConn) SetWriteDeadline(_ time.Time) error { return nil }

// mockDialer implements Dialer for testing
type mockDialer struct {
	conn *mockConn
	err  error
}

func (m *mockDialer) Dial(_, _ string, _ time.Duration) (net.Conn, error) {
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
		if status != StatusRunning {
			t.Errorf("GetStatus() = %q, want %q", status, StatusRunning)
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

// --- Error Handling Tests ---

// errorConn implements net.Conn that fails on operations
type errorConn struct {
	readErr  error
	writeErr error
	closed   bool
}

func (e *errorConn) Read(_ []byte) (n int, err error) {
	if e.readErr != nil {
		return 0, e.readErr
	}
	return 0, net.ErrClosed
}

func (e *errorConn) Write(b []byte) (n int, err error) {
	if e.writeErr != nil {
		return 0, e.writeErr
	}
	return len(b), nil
}

func (e *errorConn) Close() error                       { e.closed = true; return nil }
func (e *errorConn) LocalAddr() net.Addr                { return nil }
func (e *errorConn) RemoteAddr() net.Addr               { return nil }
func (e *errorConn) SetDeadline(_ time.Time) error      { return nil }
func (e *errorConn) SetReadDeadline(_ time.Time) error  { return nil }
func (e *errorConn) SetWriteDeadline(_ time.Time) error { return nil }

// errorDialer returns errors or faulty connections
type errorDialer struct {
	dialErr error
	conn    net.Conn
}

func (e *errorDialer) Dial(_, _ string, _ time.Duration) (net.Conn, error) {
	if e.dialErr != nil {
		return nil, e.dialErr
	}
	return e.conn, nil
}

func TestClientDialError(t *testing.T) {
	// Use an error that isSocketNotAvailable recognizes
	socketNotFoundErr := fmt.Errorf("dial unix /nonexistent: no such file or directory")
	dialer := &errorDialer{dialErr: socketNotFoundErr}
	client := NewClientWithDialer("/tmp/test", dialer)

	t.Run("IsRunning returns false on dial error", func(t *testing.T) {
		if client.IsRunning() {
			t.Error("IsRunning() = true, want false on dial error")
		}
	})

	t.Run("Stop returns error on dial error", func(t *testing.T) {
		err := client.StopVM()
		if err == nil {
			t.Error("StopVM() = nil, want error on dial error")
		}
	})

	t.Run("Status returns stopped on dial error", func(t *testing.T) {
		status, err := client.GetStatus()
		if err != nil {
			t.Errorf("GetStatus() error = %v, want nil", err)
		}
		if status != StatusStopped {
			t.Errorf("GetStatus() = %q, want %q", status, StatusStopped)
		}
	})
}

func TestClientReadError(t *testing.T) {
	t.Run("read error during ping", func(t *testing.T) {
		conn := &errorConn{readErr: net.ErrClosed}
		dialer := &errorDialer{conn: conn}
		client := NewClientWithDialer("/tmp/test", dialer)

		if client.IsRunning() {
			t.Error("IsRunning() = true, want false on read error")
		}
	})

	t.Run("read error during stop", func(t *testing.T) {
		conn := &errorConn{readErr: net.ErrClosed}
		dialer := &errorDialer{conn: conn}
		client := NewClientWithDialer("/tmp/test", dialer)

		err := client.StopVM()
		if err == nil {
			t.Error("StopVM() = nil, want error on read error")
		}
	})
}

func TestClientWriteError(t *testing.T) {
	conn := &errorConn{writeErr: net.ErrClosed}
	dialer := &errorDialer{conn: conn}
	client := NewClientWithDialer("/tmp/test", dialer)

	err := client.StopVM()
	if err == nil {
		t.Error("StopVM() = nil, want error on write error")
	}
}

func TestClientMalformedResponse(t *testing.T) {
	t.Run("error response from server", func(t *testing.T) {
		conn := &mockConn{readData: []byte("error: something went wrong\n")}
		dialer := &mockDialer{conn: conn}
		client := NewClientWithDialer("/tmp/test", dialer)

		err := client.StopVM()
		if err == nil {
			t.Error("StopVM() = nil, want error")
		}
		if err != nil && err.Error() != "server error: something went wrong" {
			t.Errorf("error = %q, want %q", err.Error(), "server error: something went wrong")
		}
	})

	t.Run("unexpected response", func(t *testing.T) {
		conn := &mockConn{readData: []byte("unexpected\n")}
		dialer := &mockDialer{conn: conn}
		client := NewClientWithDialer("/tmp/test", dialer)

		err := client.StopVM()
		if err == nil {
			t.Error("StopVM() = nil, want error for unexpected response")
		}
	})
}

func TestServerCtxCancel(t *testing.T) {
	// Use /tmp for shorter socket path (Unix sockets have 108 char limit)
	tmpDir, err := os.MkdirTemp("/tmp", "ctrl-test-")
	if err != nil {
		t.Fatalf("MkdirTemp() error = %v", err)
	}
	defer os.RemoveAll(tmpDir)

	server, err := NewServer(tmpDir, func() {})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())

	// Track when Start returns
	done := make(chan struct{})
	go func() {
		server.Start(ctx)
		close(done)
	}()

	// Give server time to start
	time.Sleep(50 * time.Millisecond)

	// Verify server is running
	client := NewClient(tmpDir)
	if !client.IsRunning() {
		t.Fatal("server not running before cancel")
	}

	// Cancel context, then close server to unblock Accept()
	// (context alone doesn't unblock Accept; closing the listener does)
	cancel()
	server.Close()

	select {
	case <-done:
		// Good - Start returned
	case <-time.After(2 * time.Second):
		t.Error("Start did not exit after Close()")
	}
}

// --- Concurrency Tests ---

func TestConcurrentClients(t *testing.T) {
	tmpDir := t.TempDir()

	var stopCount atomic.Int32
	server, err := NewServer(tmpDir, func() {
		stopCount.Add(1)
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go server.Start(ctx)
	time.Sleep(50 * time.Millisecond)

	const numClients = 20
	const opsPerClient = 10

	// Track errors
	var pingErrors, statusErrors atomic.Int32
	var successfulPings, successfulStatuses atomic.Int32

	done := make(chan struct{})
	for i := 0; i < numClients; i++ {
		go func() {
			client := NewClient(tmpDir)
			for j := 0; j < opsPerClient; j++ {
				// Alternate between ping and status
				if j%2 == 0 {
					if client.IsRunning() {
						successfulPings.Add(1)
					} else {
						pingErrors.Add(1)
					}
				} else {
					status, err := client.GetStatus()
					if err != nil || status != StatusRunning {
						statusErrors.Add(1)
					} else {
						successfulStatuses.Add(1)
					}
				}
			}
			done <- struct{}{}
		}()
	}

	// Wait for all clients
	for i := 0; i < numClients; i++ {
		<-done
	}

	totalOps := numClients * opsPerClient
	t.Logf("Successful pings: %d, status: %d", successfulPings.Load(), successfulStatuses.Load())
	t.Logf("Errors - ping: %d, status: %d", pingErrors.Load(), statusErrors.Load())

	// Allow small error rate (network timing issues)
	errorRate := float64(pingErrors.Load()+statusErrors.Load()) / float64(totalOps)
	if errorRate > 0.05 {
		t.Errorf("error rate = %.2f%%, want < 5%%", errorRate*100)
	}
}

func TestConcurrentStopRace(t *testing.T) {
	tmpDir := t.TempDir()

	var stopCount atomic.Int32
	server, err := NewServer(tmpDir, func() {
		stopCount.Add(1)
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go server.Start(ctx)
	time.Sleep(50 * time.Millisecond)

	// Multiple clients trying to stop simultaneously
	const numClients = 10
	done := make(chan error, numClients)

	for i := 0; i < numClients; i++ {
		go func() {
			client := NewClient(tmpDir)
			done <- client.StopVM()
		}()
	}

	// Collect results
	var successCount, errorCount int
	for i := 0; i < numClients; i++ {
		if err := <-done; err != nil {
			errorCount++
		} else {
			successCount++
		}
	}

	t.Logf("Stop results - success: %d, errors: %d", successCount, errorCount)

	// At least one should succeed; stopFunc should be called exactly once
	if successCount == 0 {
		t.Error("no Stop() calls succeeded")
	}
	if stopCount.Load() != 1 {
		t.Errorf("stopFunc called %d times, want 1", stopCount.Load())
	}
}

func TestRapidConnectDisconnect(t *testing.T) {
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

	// Rapid connect/disconnect cycles
	const iterations = 50
	var successCount atomic.Int32

	for i := 0; i < iterations; i++ {
		client := NewClient(tmpDir)
		if client.IsRunning() {
			successCount.Add(1)
		}
	}

	// Most should succeed
	if successCount.Load() < int32(iterations*0.9) {
		t.Errorf("success rate = %d/%d, want >= 90%%", successCount.Load(), iterations)
	}
}

// --- Wire Format Tests ---

func TestWireFormatEncodeDecode(t *testing.T) {
	t.Run("LineFormat command", func(t *testing.T) {
		format := LineFormat{}
		var buf mockBuffer
		msg := &Message{Command: "test"}

		if err := format.Encode(&buf, msg); err != nil {
			t.Fatalf("Encode() error = %v", err)
		}
		if buf.String() != "test\n" {
			t.Errorf("Encode() = %q, want %q", buf.String(), "test\n")
		}
	})

	t.Run("LineFormat response", func(t *testing.T) {
		format := LineFormat{}
		var buf mockBuffer
		msg := &Message{Response: "ok"}

		if err := format.Encode(&buf, msg); err != nil {
			t.Fatalf("Encode() error = %v", err)
		}
		if buf.String() != "ok\n" {
			t.Errorf("Encode() = %q, want %q", buf.String(), "ok\n")
		}
	})

	t.Run("LineFormat error", func(t *testing.T) {
		format := LineFormat{}
		var buf mockBuffer
		msg := &Message{Error: "something failed"}

		if err := format.Encode(&buf, msg); err != nil {
			t.Fatalf("Encode() error = %v", err)
		}
		if buf.String() != "error: something failed\n" {
			t.Errorf("Encode() = %q, want %q", buf.String(), "error: something failed\n")
		}
	})

	t.Run("LineFormat empty message error", func(t *testing.T) {
		format := LineFormat{}
		var buf mockBuffer
		msg := &Message{}

		if err := format.Encode(&buf, msg); err == nil {
			t.Error("Encode() = nil, want error for empty message")
		}
	})

	t.Run("JSONFormat roundtrip", func(t *testing.T) {
		format := JSONFormat{}
		original := &Message{Command: "test", Response: "value"}

		var buf mockBuffer
		if err := format.Encode(&buf, original); err != nil {
			t.Fatalf("Encode() error = %v", err)
		}

		decoded, err := format.Decode(&buf)
		if err != nil {
			t.Fatalf("Decode() error = %v", err)
		}

		if decoded.Command != original.Command {
			t.Errorf("Command = %q, want %q", decoded.Command, original.Command)
		}
		if decoded.Response != original.Response {
			t.Errorf("Response = %q, want %q", decoded.Response, original.Response)
		}
	})
}

// mockBuffer implements io.Reader and io.Writer for testing
type mockBuffer struct {
	data []byte
	pos  int
}

func (m *mockBuffer) Write(p []byte) (n int, err error) {
	m.data = append(m.data, p...)
	return len(p), nil
}

func (m *mockBuffer) Read(p []byte) (n int, err error) {
	if m.pos >= len(m.data) {
		return 0, net.ErrClosed
	}
	n = copy(p, m.data[m.pos:])
	m.pos += n
	return n, nil
}

func (m *mockBuffer) String() string {
	return string(m.data)
}

// --- LocalController Tests ---

func TestLocalController(t *testing.T) {
	t.Run("Ping always succeeds", func(t *testing.T) {
		ctrl := NewLocalController(func() {})
		if err := ctrl.Ping(context.Background()); err != nil {
			t.Errorf("Ping() error = %v", err)
		}
	})

	t.Run("Status before stop is running", func(t *testing.T) {
		ctrl := NewLocalController(func() {})
		status, err := ctrl.Status(context.Background())
		if err != nil {
			t.Errorf("Status() error = %v", err)
		}
		if status != StatusRunning {
			t.Errorf("Status() = %q, want %q", status, StatusRunning)
		}
	})

	t.Run("Status after stop is stopped", func(t *testing.T) {
		ctrl := NewLocalController(func() {})
		_ = ctrl.Stop(context.Background())
		status, err := ctrl.Status(context.Background())
		if err != nil {
			t.Errorf("Status() error = %v", err)
		}
		if status != StatusStopped {
			t.Errorf("Status() = %q, want %q", status, StatusStopped)
		}
	})

	t.Run("Stop calls stopFunc once", func(t *testing.T) {
		var callCount atomic.Int32
		ctrl := NewLocalController(func() {
			callCount.Add(1)
		})

		// Call stop multiple times
		_ = ctrl.Stop(context.Background())
		_ = ctrl.Stop(context.Background())
		_ = ctrl.Stop(context.Background())

		if callCount.Load() != 1 {
			t.Errorf("stopFunc called %d times, want 1", callCount.Load())
		}
	})

	t.Run("IsStopped reflects state", func(t *testing.T) {
		ctrl := NewLocalController(func() {})

		if ctrl.IsStopped() {
			t.Error("IsStopped() = true before Stop(), want false")
		}

		_ = ctrl.Stop(context.Background())

		if !ctrl.IsStopped() {
			t.Error("IsStopped() = false after Stop(), want true")
		}
	})

	t.Run("nil stopFunc is safe", func(t *testing.T) {
		ctrl := NewLocalController(nil)
		if err := ctrl.Stop(context.Background()); err != nil {
			t.Errorf("Stop() with nil func error = %v", err)
		}
	})
}

// --- Controller Interface Tests ---

func TestControllerFunc(t *testing.T) {
	t.Run("custom ping", func(t *testing.T) {
		ctrl := ControllerFunc{
			PingFn: func(ctx context.Context) error {
				return context.DeadlineExceeded
			},
		}
		if err := ctrl.Ping(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Ping() = %v, want %v", err, context.DeadlineExceeded)
		}
	})

	t.Run("nil functions use defaults", func(t *testing.T) {
		ctrl := ControllerFunc{}

		if err := ctrl.Ping(context.Background()); err != nil {
			t.Errorf("Ping() = %v, want nil", err)
		}

		status, err := ctrl.Status(context.Background())
		if err != nil {
			t.Errorf("Status() error = %v", err)
		}
		if status != StatusRunning {
			t.Errorf("Status() = %q, want %q", status, StatusRunning)
		}

		if err := ctrl.Stop(context.Background()); err != nil {
			t.Errorf("Stop() = %v, want nil", err)
		}
	})
}
