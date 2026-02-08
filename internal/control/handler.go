package control

import (
	"context"
	"fmt"
	"strings"
)

// Request represents an incoming command with optional arguments.
type Request struct {
	Command string            // command name, e.g. "stop" or "config.get"
	Args    map[string]string // key-value arguments
	Raw     string            // original unparsed payload
}

// NewRequest creates a request from a command string.
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

	// Parse remaining parts as key=value or positional args
	for i, part := range parts[1:] {
		if idx := strings.Index(part, "="); idx > 0 {
			req.Args[part[:idx]] = part[idx+1:]
		} else {
			// Positional args stored by index
			req.Args[fmt.Sprintf("%d", i)] = part
		}
	}

	return req
}

// Handler processes a request and returns a message response.
type Handler interface {
	Handle(ctx context.Context, req *Request) *Message
}

// HandlerFunc is a function adapter for Handler.
type HandlerFunc func(ctx context.Context, req *Request) *Message

// Handle implements Handler.
func (f HandlerFunc) Handle(ctx context.Context, req *Request) *Message {
	return f(ctx, req)
}

// Router dispatches requests to handlers based on command patterns.
type Router struct {
	handlers map[string]Handler
	prefix   map[string]*Router // nested routers for namespaced commands
}

// NewRouter creates an empty router.
func NewRouter() *Router {
	return &Router{
		handlers: make(map[string]Handler),
		prefix:   make(map[string]*Router),
	}
}

// Handle registers a handler for a command.
func (r *Router) Handle(command string, h Handler) {
	r.handlers[command] = h
}

// HandleFunc registers a handler function for a command.
func (r *Router) HandleFunc(command string, f HandlerFunc) {
	r.handlers[command] = f
}

// Mount attaches a sub-router for a command prefix.
// Commands like "config.get" route to the "config" sub-router.
func (r *Router) Mount(prefix string, sub *Router) {
	r.prefix[prefix] = sub
}

// Dispatch routes a request to the appropriate handler.
func (r *Router) Dispatch(ctx context.Context, req *Request) *Message {
	// Check for exact command match first
	if h, ok := r.handlers[req.Command]; ok {
		return h.Handle(ctx, req)
	}

	// Check for namespaced command (e.g., "config.get" -> prefix "config", cmd "get")
	if idx := strings.Index(req.Command, "."); idx > 0 {
		prefix := req.Command[:idx]
		if sub, ok := r.prefix[prefix]; ok {
			// Create sub-request with remaining command
			subReq := &Request{
				Command: req.Command[idx+1:],
				Args:    req.Args,
				Raw:     req.Raw,
			}
			return sub.Dispatch(ctx, subReq)
		}
	}

	return &Message{Error: fmt.Sprintf("unknown command: %s", req.Command)}
}

// Commands returns all registered command names including mounted prefixes.
func (r *Router) Commands() []string {
	cmds := make([]string, 0, len(r.handlers)+len(r.prefix))
	for cmd := range r.handlers {
		cmds = append(cmds, cmd)
	}
	for prefix, sub := range r.prefix {
		for _, cmd := range sub.Commands() {
			cmds = append(cmds, prefix+"."+cmd)
		}
	}
	return cmds
}

// --- Backward compatibility with existing CommandRegistry usage ---

// CommandRegistry is deprecated, use Router instead.
// Kept for backward compatibility during migration.
type CommandRegistry struct {
	router *Router
}

// NewCommandRegistry creates a registry backed by a Router.
func NewCommandRegistry() *CommandRegistry {
	return &CommandRegistry{router: NewRouter()}
}

// Register adds a simple handler (no arguments).
func (r *CommandRegistry) Register(command string, handler func() *Message) {
	r.router.HandleFunc(command, func(_ context.Context, _ *Request) *Message {
		return handler()
	})
}

// Handle dispatches using the backing router.
func (r *CommandRegistry) Handle(cmd string) *Message {
	return r.router.Dispatch(context.Background(), NewRequest(cmd))
}

// Has returns true if handler exists.
func (r *CommandRegistry) Has(command string) bool {
	_, ok := r.router.handlers[command]
	return ok
}

// Commands returns registered commands.
func (r *CommandRegistry) Commands() []string {
	return r.router.Commands()
}
