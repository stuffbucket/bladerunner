// Package control provides a VM control plane with pluggable transports and wire formats.
//
// Architecture:
//
//	Controller (domain) ─────┐
//	                         v
//	Client ─── WireFormat ─── Transport ─── Listener ─── Router ─── Handler ─── Controller
//
// Components:
//   - Controller: Domain interface defining VM operations (Stop, Status, etc.)
//   - WireFormat: Serialization format (line-based, JSON, etc.)
//   - Transport:  Connection mechanism (Unix socket, TCP, etc.)
//   - Router:     Dispatches commands to handlers
//   - Listener:   Accepts connections and coordinates components
//   - Client:     Sends commands to a listener
package control

import "context"

// Controller defines domain operations for VM lifecycle management.
// Implementations can be local (direct VM control) or remote (RPC proxy).
type Controller interface {
	// Ping checks if the controller is responsive.
	Ping(ctx context.Context) error
	// Status returns the current VM status.
	Status(ctx context.Context) (string, error)
	// Stop gracefully shuts down the VM.
	Stop(ctx context.Context) error
}

// ControllerFunc allows functions to implement single Controller methods.
// Useful for testing or composition.
type ControllerFunc struct {
	PingFn   func(ctx context.Context) error
	StatusFn func(ctx context.Context) (string, error)
	StopFn   func(ctx context.Context) error
}

// Ping implements Controller.
func (f ControllerFunc) Ping(ctx context.Context) error {
	if f.PingFn != nil {
		return f.PingFn(ctx)
	}
	return nil
}

// Status implements Controller.
func (f ControllerFunc) Status(ctx context.Context) (string, error) {
	if f.StatusFn != nil {
		return f.StatusFn(ctx)
	}
	return StatusRunning, nil
}

// Stop implements Controller.
func (f ControllerFunc) Stop(ctx context.Context) error {
	if f.StopFn != nil {
		return f.StopFn(ctx)
	}
	return nil
}

// Status constants
const (
	StatusRunning = "running"
	StatusStopped = "stopped"
)

// Command constants
const (
	CmdPing   = "ping"
	CmdStop   = "stop"
	CmdStatus = "status"
)

// Response constants
const (
	RespOK   = "ok"
	RespPong = "pong"
)
