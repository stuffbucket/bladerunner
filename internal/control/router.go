package control

import (
	"context"
	"fmt"
	"strings"
)

// Router dispatches command requests to registered handlers.
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
// Commands like "config.get" route to the "config" sub-router with command "get".
func (r *Router) Mount(prefix string, sub *Router) {
	r.prefix[prefix] = sub
}

// Dispatch routes a request to the appropriate handler.
func (r *Router) Dispatch(ctx context.Context, req *Request) *Message {
	// Check for exact command match
	if h, ok := r.handlers[req.Command]; ok {
		return h.Handle(ctx, req)
	}

	// Check for namespaced command (e.g., "config.get" -> prefix "config", cmd "get")
	if idx := strings.Index(req.Command, "."); idx > 0 {
		prefix := req.Command[:idx]
		if sub, ok := r.prefix[prefix]; ok {
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

// RegisterController registers standard commands that delegate to a Controller.
func (r *Router) RegisterController(ctrl Controller) {
	r.HandleFunc(CmdPing, func(ctx context.Context, _ *Request) *Message {
		if err := ctrl.Ping(ctx); err != nil {
			return &Message{Error: err.Error()}
		}
		return &Message{Response: RespPong}
	})

	r.HandleFunc(CmdStatus, func(ctx context.Context, _ *Request) *Message {
		status, err := ctrl.Status(ctx)
		if err != nil {
			return &Message{Error: err.Error()}
		}
		return &Message{Response: status}
	})

	r.HandleFunc(CmdStop, func(ctx context.Context, _ *Request) *Message {
		if err := ctrl.Stop(ctx); err != nil {
			return &Message{Error: err.Error()}
		}
		return &Message{Response: RespOK}
	})
}
