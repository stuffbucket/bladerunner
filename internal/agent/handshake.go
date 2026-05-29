package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/stuffbucket/bladerunner/internal/logging"
)

// Default timeout per command exchange on the agent control channel.
const defaultExchangeTimeout = 90 * time.Second

// HandshakeConfig describes the payloads the host will push to the guest
// during the initial connection handshake.
type HandshakeConfig struct {
	// ConfigPush is the configuration to apply via CmdConfigPush.
	ConfigPush ConfigPushArgs
	// AuthorizedKeys, when non-empty, is sent as CmdUserSync after readiness.
	AuthorizedKeys string
	// PerCommandTimeout overrides the default per-exchange timeout.
	PerCommandTimeout time.Duration
}

// HandshakeResult collects what the agent reported back during the
// handshake.
type HandshakeResult struct {
	Ready      ReadyWaitResponse
	ConfigOK   bool
	UserSyncOK bool
}

// RunHandshake drives the host-side handshake sequence over conn:
//
//  1. CmdConfigPush
//  2. CmdReadyWait
//  3. CmdUserSync (only if HandshakeConfig.AuthorizedKeys is non-empty)
//
// It returns the aggregate result or the first error. Caller owns conn.
func RunHandshake(ctx context.Context, conn net.Conn, cfg HandshakeConfig) (*HandshakeResult, error) {
	if conn == nil {
		return nil, errors.New("agent: nil conn")
	}
	timeout := cfg.PerCommandTimeout
	if timeout == 0 {
		timeout = defaultExchangeTimeout
	}
	br := bufio.NewReader(conn)
	res := &HandshakeResult{}

	// Phase 1: push config.
	pushArgs, err := EncodeArgs(cfg.ConfigPush)
	if err != nil {
		return nil, err
	}
	if _, err := exchange(ctx, conn, br, timeout, &Message{Command: CmdConfigPush, Args: pushArgs}); err != nil {
		return nil, fmt.Errorf("agent: config push: %w", err)
	}
	res.ConfigOK = true
	logging.L().Info("agent config push acknowledged")

	// Phase 2: wait for readiness, capture server info.
	readyMsg, err := exchange(ctx, conn, br, timeout, &Message{Command: CmdReadyWait})
	if err != nil {
		return nil, fmt.Errorf("agent: ready wait: %w", err)
	}
	if err := DecodeArgs(readyMsg, &res.Ready); err != nil {
		return nil, fmt.Errorf("agent: parse ready response: %w", err)
	}
	logging.L().Info("agent reported ready", "incus_version", res.Ready.IncusVersion, "api_address", res.Ready.APIAddress)

	// Phase 3: user sync, optional.
	if cfg.AuthorizedKeys == "" {
		return res, nil
	}
	userArgs, err := EncodeArgs(UserSyncArgs{AuthorizedKeys: cfg.AuthorizedKeys})
	if err != nil {
		return nil, err
	}
	if _, err := exchange(ctx, conn, br, timeout, &Message{Command: CmdUserSync, Args: userArgs}); err != nil {
		return nil, fmt.Errorf("agent: user sync: %w", err)
	}
	res.UserSyncOK = true
	logging.L().Info("agent user sync acknowledged")
	return res, nil
}

// exchange sends one command and reads one response, returning the response
// or surfacing any error. It honors ctx cancellation via the conn deadline.
func exchange(ctx context.Context, conn net.Conn, br *bufio.Reader, timeout time.Duration, cmd *Message) (*Message, error) {
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("set deadline: %w", err)
	}
	if err := EncodeMessage(conn, cmd); err != nil {
		return nil, err
	}
	resp, err := DecodeMessage(br)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("agent disconnected before reply: %w", err)
		}
		return nil, err
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("agent error: %s", resp.Error)
	}
	return resp, nil
}
