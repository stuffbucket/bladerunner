//go:build !darwin

package main

// Off macOS there is no DiskArbitration, so nothing can be noticed being
// mounted. The API is kept identical so watch.go stays platform-neutral and
// GOOS=linux keeps building; the error is explicit rather than a silent watch
// that never fires.

import (
	"fmt"

	"github.com/stuffbucket/bladerunner/internal/diskarb"
)

// startCartridgeWatch reports that mount detection is unavailable on this
// platform. The error wraps diskarb.ErrUnsupported (and so errors.ErrUnsupported).
func startCartridgeWatch(_ *cartridgeWatcher) (stop func(), err error) {
	return nil, fmt.Errorf("watch for cartridges: %w", diskarb.ErrUnsupported)
}
