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

// Status implements Controller.
func (c *LocalController) Status(_ context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return StatusStopped, nil
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
