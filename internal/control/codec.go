package control

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Message represents a protocol message (command or response).
type Message struct {
	Command  string `json:"command,omitempty"`
	Response string `json:"response,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Codec handles message encoding and decoding.
// Implementations can provide line-based, JSON, protobuf, etc.
type Codec interface {
	// Encode writes a message to the writer
	Encode(w io.Writer, msg *Message) error
	// Decode reads a message from the reader
	Decode(r io.Reader) (*Message, error)
}

// LineCodec implements a simple newline-delimited text protocol.
// Commands: "ping\n", "stop\n", "status\n"
// Responses: "ok\n", "pong\n", "running\n", "error: message\n"
type LineCodec struct{}

// Encode writes a message as a newline-terminated string.
func (LineCodec) Encode(w io.Writer, msg *Message) error {
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
	_, err := w.Write([]byte(line + "\n"))
	return err
}

// Decode reads a newline-terminated message.
func (LineCodec) Decode(r io.Reader) (*Message, error) {
	reader := bufio.NewReader(r)
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	text := strings.TrimSpace(line)

	// Parse as error response
	if strings.HasPrefix(text, "error: ") {
		return &Message{Error: strings.TrimPrefix(text, "error: ")}, nil
	}

	// Could be a command or response - context determines interpretation
	return &Message{Response: text, Command: text}, nil
}

// JSONCodec implements a JSON-based protocol.
// Each message is a single JSON object followed by a newline.
type JSONCodec struct{}

// Encode writes a message as JSON.
func (JSONCodec) Encode(w io.Writer, msg *Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

// Decode reads a JSON message.
func (JSONCodec) Decode(r io.Reader) (*Message, error) {
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

// DefaultCodec is the codec used by default (line-based).
var DefaultCodec Codec = LineCodec{}
