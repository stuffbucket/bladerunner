package control

import (
	"context"
	"fmt"
	"strings"
)

// Request represents a parsed command with optional arguments.
type Request struct {
	Command string            // command name, e.g. "stop" or "config.get"
	Args    map[string]string // key-value arguments
	Raw     string            // original unparsed payload
}

// NewRequest parses a command string into a Request.
// Supports formats: "command", "command arg1 arg2", "command key=value"
func NewRequest(raw string) *Request {
	parts := strings.Fields(raw)
	if len(parts) == 0 {
		return &Request{Raw: raw}
	}

	req := &Request{
		Command: parts[0],
		Args:    make(map[string]string),
		Raw:     raw,
	}

	for i, part := range parts[1:] {
		if idx := strings.Index(part, "="); idx > 0 {
			req.Args[part[:idx]] = part[idx+1:]
		} else {
			req.Args[fmt.Sprintf("%d", i)] = part
		}
	}

	return req
}

// Handler processes a command request and returns a response.
type Handler interface {
	Handle(ctx context.Context, req *Request) *Message
}

// HandlerFunc adapts a function to the Handler interface.
type HandlerFunc func(ctx context.Context, req *Request) *Message

// Handle implements Handler.
func (f HandlerFunc) Handle(ctx context.Context, req *Request) *Message {
	return f(ctx, req)
}
