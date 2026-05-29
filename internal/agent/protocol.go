// Package agent implements the host side of the bladerunner guest control
// agent (br-agent) protocol. The protocol is a line-delimited JSON exchange
// over vsock: the host listens on a vsock port and the in-guest agent dials
// it on boot. After the connection is established the host drives the
// handshake by sending commands; the agent applies them and replies.
//
// This package is import-safe on all platforms; only the listener (in
// listener_darwin.go) depends on the macOS virtualization framework.
package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Protocol version negotiated between host and guest. The agent must reject
// any version higher than it understands and reply with an error.
const ProtocolVersion = 1

// Command names spoken by the host. Constants are exported so both the host
// runner and the cmd/br-agent binary can reference them.
const (
	// CmdConfigPush instructs the agent to write
	// /etc/bladerunner/config.json, apply Incus config keys, and reload any
	// affected systemd units. Args is a free-form JSON object whose keys are
	// documented in ConfigPushArgs.
	CmdConfigPush = "agent.config.push"

	// CmdReadyWait asks the agent to poll `incus admin waitready`, run
	// `incus admin init --auto` if Incus has not been initialized, and reply
	// with the running server's information in ReadyWaitResponse.
	CmdReadyWait = "agent.ready.wait"

	// CmdUserSync atomically rewrites the incus user's authorized_keys file
	// with the keys supplied in UserSyncArgs.AuthorizedKeys.
	CmdUserSync = "agent.user.sync"
)

// Response status strings.
const (
	ResponseOK    = "ok"
	ResponseReady = "ready"
)

// Message is the on-the-wire representation of a single protocol frame.
// Either Command or Response is set; Error is set on failure.
type Message struct {
	Version  int             `json:"version"`
	Command  string          `json:"command,omitempty"`
	Response string          `json:"response,omitempty"`
	Error    string          `json:"error,omitempty"`
	Args     json.RawMessage `json:"args,omitempty"`
}

// ConfigPushArgs is the payload pushed by the host on CmdConfigPush.
// All fields are optional: a missing field means "do not change".
type ConfigPushArgs struct {
	OIDCIssuer   string `json:"oidc_issuer,omitempty"`
	OIDCClientID string `json:"oidc_client_id,omitempty"`
	OIDCAudience string `json:"oidc_audience,omitempty"`
	// CoreHTTPSAddress is the value to set for core.https_address on Incus.
	// Typically "[::]:8443".
	CoreHTTPSAddress string `json:"core_https_address,omitempty"`
}

// ReadyWaitResponse is the JSON-encoded body returned in Message.Args when
// the guest is ready. Fields mirror a subset of incus's GetServer output.
type ReadyWaitResponse struct {
	IncusVersion string `json:"incus_version,omitempty"`
	APIAddress   string `json:"api_address,omitempty"`
}

// UserSyncArgs is the payload pushed by the host on CmdUserSync.
type UserSyncArgs struct {
	// AuthorizedKeys is the full desired contents of
	// ~incus/.ssh/authorized_keys (one key per line, trailing newline
	// optional). It atomically replaces the existing file.
	AuthorizedKeys string `json:"authorized_keys"`
}

// ErrUnsupportedVersion is returned when a peer advertises a protocol
// version newer than ProtocolVersion.
var ErrUnsupportedVersion = errors.New("agent: unsupported protocol version")

// EncodeMessage serializes a message and writes it followed by a newline.
func EncodeMessage(w io.Writer, msg *Message) error {
	if msg.Version == 0 {
		msg.Version = ProtocolVersion
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if _, err := w.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// DecodeMessage reads a newline-terminated JSON message from r.
func DecodeMessage(r *bufio.Reader) (*Message, error) {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var msg Message
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	if msg.Version > ProtocolVersion {
		return &msg, fmt.Errorf("%w: peer=%d max=%d", ErrUnsupportedVersion, msg.Version, ProtocolVersion)
	}
	return &msg, nil
}

// DecodeArgs unmarshals msg.Args into v. Returns nil if Args is empty.
func DecodeArgs(msg *Message, v any) error {
	if len(msg.Args) == 0 {
		return nil
	}
	if err := json.Unmarshal(msg.Args, v); err != nil {
		return fmt.Errorf("decode args: %w", err)
	}
	return nil
}

// EncodeArgs marshals v into a json.RawMessage for use as Message.Args.
func EncodeArgs(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode args: %w", err)
	}
	return data, nil
}
