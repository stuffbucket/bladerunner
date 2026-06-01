package control

import (
	"context"
	"sync"
)

// LocalController implements Controller for a local VM instance.
// It wraps a stop function and provides thread-safe access.
type LocalController struct {
	stopFunc func()
	mu       sync.Mutex
	stopped  bool
	// probe optionally checks guest liveness. When set and the controller is
	// not stopped, Status calls it: a nil error means the guest answers and
	// Status reports StatusRunning; a non-nil error reports StatusUnreachable.
	// When nil, Status reports StatusRunning based on host run-state alone.
	probe func(context.Context) error
}

// NewLocalController creates a controller with the given stop function.
func NewLocalController(stopFunc func()) *LocalController {
	return &LocalController{
		stopFunc: stopFunc,
	}
}

// Ping implements Controller.
func (c *LocalController) Ping(_ context.Context) error {
	return nil
}

// SetProbe attaches a guest-liveness probe. It is safe to call after the
// controller is serving (e.g. once the VM has started and its vsock device
// exists). Passing nil clears the probe, reverting to host-run-state-only
// status reporting.
func (c *LocalController) SetProbe(probe func(context.Context) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.probe = probe
}

// Status implements Controller.
func (c *LocalController) Status(ctx context.Context) (string, error) {
	c.mu.Lock()
	stopped := c.stopped
	probe := c.probe
	c.mu.Unlock()

	if stopped {
		return StatusStopped, nil
	}
	// Run the probe outside the lock so a slow/blocking guest dial does not
	// stall Stop() or other status callers. A probe failure is mapped to a
	// status string (unreachable), not surfaced as a Status error.
	guestReachable := true
	if probe != nil {
		guestReachable = probe(ctx) == nil
	}
	if !guestReachable {
		return StatusUnreachable, nil
	}
	return StatusRunning, nil
}

// Stop implements Controller.
func (c *LocalController) Stop(_ context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return nil
	}
	c.stopped = true
	if c.stopFunc != nil {
		c.stopFunc()
	}
	return nil
}

// IsStopped returns true if Stop has been called.
func (c *LocalController) IsStopped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopped
}
