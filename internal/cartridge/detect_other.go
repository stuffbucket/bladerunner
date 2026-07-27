//go:build !darwin

package cartridge

// mountReadOnly is a darwin concern: read-only-ness here means "this volume is
// a mounted UDZO disk image", and there are no disk images off macOS. It
// reports ErrUnsupported so Detect leaves ReadOnly at its zero value rather
// than asserting anything about a foreign filesystem.
func mountReadOnly(_ string) (bool, error) { return false, ErrUnsupported }
