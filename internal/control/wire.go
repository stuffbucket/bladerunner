package control

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Message represents a control protocol message.
type Message struct {
	Version  int    `json:"version,omitempty"`
	Command  string `json:"command,omitempty"`
	Response string `json:"response,omitempty"`
	Error    string `json:"error,omitempty"`
}

// WireFormat handles message serialization for the control protocol.
// Implementations provide different encoding strategies (text, JSON, etc.).
type WireFormat interface {
	// Encode writes a message to the writer.
	Encode(w io.Writer, msg *Message) error
	// Decode reads a message from the reader.
	Decode(r io.Reader) (*Message, error)
}

// LineFormat implements a simple newline-delimited text protocol.
// Version 1+: "v1 ping\n", "v1 pong\n", "v1 error: message\n"
// Legacy (v0): "ping\n", "pong\n", "error: message\n"
type LineFormat struct{}

// Encode writes a message as a newline-terminated string with version prefix.
func (LineFormat) Encode(w io.Writer, msg *Message) error {
	var line string
	switch {
	case msg.Error != "":
		line = fmt.Sprintf("error: %s", msg.Error)
	case msg.Response != "":
		line = msg.Response
	case msg.Command != "":
		line = msg.Command
	default:
		return fmt.Errorf("empty message")
	}
	if msg.Version > 0 {
		line = fmt.Sprintf("v%d %s", msg.Version, line)
	}
	_, err := w.Write([]byte(line + "\n"))
	return err
}

// Decode reads a newline-terminated message and extracts the version prefix.
func (LineFormat) Decode(r io.Reader) (*Message, error) {
	reader := bufio.NewReader(r)
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	text := strings.TrimSpace(line)

	// Extract version prefix: "v1 ..." -> version=1, text="..."
	version := 0
	if len(text) > 1 && text[0] == 'v' {
		if idx := strings.IndexByte(text, ' '); idx > 0 {
			if v, err := strconv.Atoi(text[1:idx]); err == nil {
				version = v
				text = text[idx+1:]
			}
		}
	}

	// Parse error response
	if errMsg, found := strings.CutPrefix(text, "error: "); found {
		return &Message{Version: version, Error: errMsg}, nil
	}

	// Context determines if this is command or response
	return &Message{Version: version, Response: text, Command: text}, nil
}

// JSONFormat implements a JSON-based wire format.
// Each message is a single JSON object followed by a newline.
type JSONFormat struct{}

// Encode writes a message as JSON.
func (JSONFormat) Encode(w io.Writer, msg *Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

// Decode reads a JSON message.
func (JSONFormat) Decode(r io.Reader) (*Message, error) {
	reader := bufio.NewReader(r)
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	var msg Message
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// DefaultWireFormat is the wire format used by default (line-based).
var DefaultWireFormat WireFormat = LineFormat{}

// Backward compatibility aliases
type (
	// Codec is deprecated, use WireFormat instead.
	Codec = WireFormat
	// LineCodec is deprecated, use LineFormat instead.
	LineCodec = LineFormat
	// JSONCodec is deprecated, use JSONFormat instead.
	JSONCodec = JSONFormat
)

// DefaultCodec is deprecated, use DefaultWireFormat instead.
var DefaultCodec = DefaultWireFormat
