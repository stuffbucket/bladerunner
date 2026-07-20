package agent

import (
	"bufio"
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

const testIssuer = "http://issuer"

// fakeAgent simulates an in-guest br-agent that replies to commands over a
// net.Pipe connection. It records the args it received so the test can
// assert on them.
type fakeAgent struct {
	conn net.Conn
	t    *testing.T

	mu             sync.Mutex
	gotConfigPush  ConfigPushArgs
	gotUserSync    UserSyncArgs
	configReceived bool
	userReceived   bool
}

func (f *fakeAgent) serve() {
	defer func() { _ = f.conn.Close() }()
	br := bufio.NewReader(f.conn)
	for {
		msg, err := DecodeMessage(br)
		if err != nil {
			return
		}
		switch msg.Command {
		case CmdConfigPush:
			f.mu.Lock()
			_ = DecodeArgs(msg, &f.gotConfigPush)
			f.configReceived = true
			f.mu.Unlock()
			_ = EncodeMessage(f.conn, &Message{Response: ResponseOK})
		case CmdReadyWait:
			args, _ := EncodeArgs(ReadyWaitResponse{IncusVersion: "6.0", APIAddress: "[::]:8443"})
			_ = EncodeMessage(f.conn, &Message{Response: ResponseReady, Args: args})
		case CmdUserSync:
			f.mu.Lock()
			_ = DecodeArgs(msg, &f.gotUserSync)
			f.userReceived = true
			f.mu.Unlock()
			_ = EncodeMessage(f.conn, &Message{Response: ResponseOK})
		default:
			_ = EncodeMessage(f.conn, &Message{Error: "unknown command"})
		}
	}
}

func TestRunHandshakeFullSequence(t *testing.T) {
	hostConn, guestConn := net.Pipe()
	fa := &fakeAgent{conn: guestConn, t: t}
	go fa.serve()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := RunHandshake(ctx, hostConn, HandshakeConfig{
		ConfigPush: ConfigPushArgs{
			OIDCIssuer:   testIssuer,
			OIDCClientID: "bladerunner",
			OIDCAudience: "bladerunner",
		},
		AuthorizedKeys: "ssh-ed25519 AAAA test",
	})
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if !res.ConfigOK || !res.UserSyncOK {
		t.Fatalf("incomplete result: %+v", res)
	}
	if res.Ready.IncusVersion != "6.0" {
		t.Fatalf("ready.incus_version = %q", res.Ready.IncusVersion)
	}

	fa.mu.Lock()
	defer fa.mu.Unlock()
	if !fa.configReceived || !fa.userReceived {
		t.Fatalf("agent did not receive both commands: config=%v user=%v", fa.configReceived, fa.userReceived)
	}
	if fa.gotConfigPush.OIDCIssuer != testIssuer {
		t.Fatalf("config.OIDCIssuer = %q", fa.gotConfigPush.OIDCIssuer)
	}
	if fa.gotUserSync.AuthorizedKeys != "ssh-ed25519 AAAA test" {
		t.Fatalf("user.AuthorizedKeys = %q", fa.gotUserSync.AuthorizedKeys)
	}
}

// TestRunHandshakeUserSyncNonFatal verifies that a failed user-sync (e.g. the
// guest's incus user not existing yet) does NOT fail the whole handshake: config
// push + ready already succeeded, and cloud-init seeds the same key anyway, so
// the host must continue on the agent path rather than drop to slow http-wait.
func TestRunHandshakeUserSyncNonFatal(t *testing.T) {
	hostConn, guestConn := net.Pipe()
	go func() {
		defer func() { _ = guestConn.Close() }()
		br := bufio.NewReader(guestConn)
		for {
			msg, err := DecodeMessage(br)
			if err != nil {
				return
			}
			switch msg.Command {
			case CmdConfigPush:
				_ = EncodeMessage(guestConn, &Message{Response: ResponseOK})
			case CmdReadyWait:
				args, _ := EncodeArgs(ReadyWaitResponse{IncusVersion: "6.0", APIAddress: "[::]:8443"})
				_ = EncodeMessage(guestConn, &Message{Response: ResponseReady, Args: args})
			case CmdUserSync:
				_ = EncodeMessage(guestConn, &Message{Error: "lookup user incus: user: unknown user incus"})
			default:
				_ = EncodeMessage(guestConn, &Message{Error: "unknown command"})
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := RunHandshake(ctx, hostConn, HandshakeConfig{
		ConfigPush:     ConfigPushArgs{OIDCIssuer: testIssuer},
		AuthorizedKeys: "ssh-ed25519 AAAA test",
	})
	if err != nil {
		t.Fatalf("handshake should not fail on user-sync error: %v", err)
	}
	if !res.ConfigOK {
		t.Fatalf("expected ConfigOK=true")
	}
	if res.UserSyncOK {
		t.Fatalf("expected UserSyncOK=false after a user-sync error")
	}
}

func TestRunHandshakeSkipsUserSyncWhenKeysEmpty(t *testing.T) {
	hostConn, guestConn := net.Pipe()
	fa := &fakeAgent{conn: guestConn, t: t}
	go fa.serve()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := RunHandshake(ctx, hostConn, HandshakeConfig{
		ConfigPush: ConfigPushArgs{OIDCIssuer: testIssuer},
	})
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if res.UserSyncOK {
		t.Fatalf("expected UserSyncOK=false when AuthorizedKeys is empty")
	}
}

func TestRunHandshakeFailsOnAgentError(t *testing.T) {
	hostConn, guestConn := net.Pipe()
	go func() {
		defer func() { _ = guestConn.Close() }()
		br := bufio.NewReader(guestConn)
		_, _ = DecodeMessage(br)
		_ = EncodeMessage(guestConn, &Message{Error: "boom"})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := RunHandshake(ctx, hostConn, HandshakeConfig{})
	if err == nil {
		t.Fatalf("expected error")
	}
}
