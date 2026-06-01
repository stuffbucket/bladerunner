//go:build !windows

package board

import (
	"os"
	"os/signal"
	"syscall"
)

// installResizeWatcher subscribes to SIGWINCH and invokes b.handleResize on
// each delivery. Returns a stop function that unregisters the handler and
// terminates the watcher goroutine. The returned function is safe to call
// from any goroutine and must be called exactly once.
func installResizeWatcher(b *Board) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	done := make(chan struct{})

	b.wg.Go(func() {
		for {
			select {
			case <-done:
				return
			case <-ch:
				b.handleResize()
			}
		}
	})

	return func() {
		signal.Stop(ch)
		close(done)
	}
}
