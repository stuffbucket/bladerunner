package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestEncodeMessageSetsVersion(t *testing.T) {
	var buf bytes.Buffer
	if err := EncodeMessage(&buf, &Message{Command: CmdConfigPush}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got Message
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Version != ProtocolVersion {
		t.Fatalf("version = %d, want %d", got.Version, ProtocolVersion)
	}
	if got.Command != CmdConfigPush {
		t.Fatalf("command = %q", got.Command)
	}
}

func TestDecodeMessageRoundTrip(t *testing.T) {
	args, err := EncodeArgs(ConfigPushArgs{OIDCIssuer: "http://example/oidc"})
	if err != nil {
		t.Fatalf("encode args: %v", err)
	}
	in := &Message{Command: CmdConfigPush, Args: args}
	var buf bytes.Buffer
	if err := EncodeMessage(&buf, in); err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := DecodeMessage(bufio.NewReader(&buf))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Command != CmdConfigPush {
		t.Fatalf("cmd = %q", out.Command)
	}
	var parsed ConfigPushArgs
	if err := DecodeArgs(out, &parsed); err != nil {
		t.Fatalf("decode args: %v", err)
	}
	if parsed.OIDCIssuer != "http://example/oidc" {
		t.Fatalf("issuer = %q", parsed.OIDCIssuer)
	}
}

func TestDecodeRejectsHigherVersion(t *testing.T) {
	raw := `{"version":99,"command":"x"}` + "\n"
	_, err := DecodeMessage(bufio.NewReader(strings.NewReader(raw)))
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("expected ErrUnsupportedVersion, got %v", err)
	}
}

func TestDecodeArgsEmpty(t *testing.T) {
	var v ConfigPushArgs
	if err := DecodeArgs(&Message{}, &v); err != nil {
		t.Fatalf("decode empty args: %v", err)
	}
	if v != (ConfigPushArgs{}) {
		t.Fatalf("expected zero value, got %+v", v)
	}
}
