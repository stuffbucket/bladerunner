//go:build windows

package board

// Windows has no SIGWINCH. bladerunner does not target windows, but the
// board package is import-safe everywhere, so we provide a no-op stub.
func installResizeWatcher(*Board) func() { return func() {} }
