package control_test

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stuffbucket/bladerunner/internal/control"
)

// nilCommand is the command name the tests below register a handler for that
// returns no message. The router must answer for it instead of passing the nil
// on to a caller that writes into the result.
const nilCommand = "nilresp"

// nilHandler is a Handler whose Handle returns nil. The Handler interface
// permits this, so the router - the registration point - must hold the
// contract that a dispatch always produces a message.
type nilHandler struct{}

// Handle implements control.Handler and deliberately returns no message.
func (nilHandler) Handle(_ context.Context, _ *control.Request) *control.Message { return nil }

// TestDispatchNilHandlerResponse holds the router contract: a handler that
// returns nil produces an error response that names the command the client
// sent, for a directly registered handler and for one behind a mounted
// sub-router. Without this the caller in listener.handleConnection writes the
// protocol version into a nil pointer and the holder process dies with the VM
// it owns.
func TestDispatchNilHandlerResponse(t *testing.T) {
	exact := control.NewRouter()
	exact.Handle(nilCommand, nilHandler{})

	sub := control.NewRouter()
	sub.HandleFunc(nilCommand, func(_ context.Context, _ *control.Request) *control.Message {
		return nil
	})
	mounted := control.NewRouter()
	mounted.Mount("nested", sub)

	cases := []struct {
		name    string
		router  *control.Router
		command string
	}{
		{"exact handler", exact, nilCommand},
		{"mounted handler", mounted, "nested." + nilCommand},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := tc.router.Dispatch(context.Background(), control.NewRequest(tc.command))
			if resp == nil {
				t.Fatalf("Dispatch(%q) = nil, want an error message", tc.command)
			}
			if resp.Error == "" {
				t.Fatalf("Dispatch(%q).Error = %q, want a nonempty error", tc.command, resp.Error)
			}
			if !strings.Contains(resp.Error, tc.command) {
				t.Errorf("Dispatch(%q).Error = %q, want it to name the command", tc.command, resp.Error)
			}
		})
	}
}

// TestDispatchKeepsHandlerResponse holds the other half of the contract: a
// handler that does return a message keeps it untouched, and an unknown
// command still reports an error.
func TestDispatchKeepsHandlerResponse(t *testing.T) {
	router := control.NewRouter()
	router.HandleFunc("echo", func(_ context.Context, _ *control.Request) *control.Message {
		return &control.Message{Response: control.RespOK}
	})

	resp := router.Dispatch(context.Background(), control.NewRequest("echo"))
	if resp == nil || resp.Response != control.RespOK {
		t.Fatalf("Dispatch(\"echo\") = %+v, want Response %q", resp, control.RespOK)
	}
	if resp.Error != "" {
		t.Errorf("Dispatch(\"echo\").Error = %q, want empty", resp.Error)
	}

	unknown := router.Dispatch(context.Background(), control.NewRequest("nosuchcommand"))
	if unknown == nil || unknown.Error == "" {
		t.Fatalf("Dispatch(\"nosuchcommand\") = %+v, want an error message", unknown)
	}
}

// TestListenerSurvivesNilHandlerResponse holds the reason the router contract
// matters: the listener runs inside the holder process that owns a live VM. A
// nil response must come back as an error on the wire, with the protocol
// version set, and the listener must still answer the next request.
func TestListenerSurvivesNilHandlerResponse(t *testing.T) {
	// A unix socket path must stay short, so /tmp rather than t.TempDir().
	dir, err := os.MkdirTemp("/tmp", "ctrl-nil-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	server, err := control.NewListener(dir, control.ControllerFunc{})
	if err != nil {
		t.Fatalf("NewListener: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	server.RegisterCommand(nilCommand, nilHandler{})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go server.Start(ctx)
	waitForListener(t, dir)

	bad := request(t, dir, nilCommand)
	if bad.Error == "" {
		t.Errorf("response to %q: Error = %q, want a nonempty error", nilCommand, bad.Error)
	}
	if bad.Version != control.ProtocolVersion {
		t.Errorf("response to %q: Version = %d, want %d", nilCommand, bad.Version, control.ProtocolVersion)
	}

	good := request(t, dir, control.CmdPing)
	if good.Response != control.RespPong {
		t.Errorf("response to %q: Response = %q, want %q", control.CmdPing, good.Response, control.RespPong)
	}
	if good.Version != control.ProtocolVersion {
		t.Errorf("response to %q: Version = %d, want %d", control.CmdPing, good.Version, control.ProtocolVersion)
	}
}

// dialTimeout bounds a single connection attempt against the test listener.
const dialTimeout = 2 * time.Second

// listenerWaitAttempts and listenerWaitStep bound the wait for the test
// listener to bind its socket.
const (
	listenerWaitAttempts = 100
	listenerWaitStep     = 20 * time.Millisecond
)

// waitForListener blocks until the listener in dir accepts a connection.
func waitForListener(t *testing.T, dir string) {
	t.Helper()
	addr := control.SocketPath(dir)
	for range listenerWaitAttempts {
		conn, err := net.DialTimeout("unix", addr, dialTimeout)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(listenerWaitStep)
	}
	t.Fatalf("listener at %s never came up", addr)
}

// request sends one command to the listener in dir and returns the response.
// It uses a raw connection because the Client exposes no way to send an
// arbitrary command name.
func request(t *testing.T, dir string, command string) *control.Message {
	t.Helper()
	conn, err := net.DialTimeout("unix", control.SocketPath(dir), dialTimeout)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(dialTimeout)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}

	format := control.LineFormat{}
	req := &control.Message{Version: control.ProtocolVersion, Command: command}
	if err := format.Encode(conn, req); err != nil {
		t.Fatalf("encode %q: %v", command, err)
	}
	resp, err := format.Decode(conn)
	if err != nil {
		t.Fatalf("decode response to %q: %v", command, err)
	}
	return resp
}
