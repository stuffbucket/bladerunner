//go:build !darwin

package diskarb

// Session is the non-darwin placeholder for a DiskArbitration session. It
// exists so code that watches for cartridge volumes compiles everywhere; every
// method reports ErrUnsupported.
type Session struct{}

// NewSession always fails off darwin: DiskArbitration is a macOS framework.
func NewSession() (*Session, error) { return nil, ErrUnsupported }

// Close is a no-op off darwin.
func (s *Session) Close() error { return nil }

// WatchAppeared always fails off darwin.
func (s *Session) WatchAppeared(func(DiskInfo)) (CancelFunc, error) {
	return nil, ErrUnsupported
}

// WatchDisappeared always fails off darwin.
func (s *Session) WatchDisappeared(func(DiskInfo)) (CancelFunc, error) {
	return nil, ErrUnsupported
}

// WatchUnmountApproval always fails off darwin.
func (s *Session) WatchUnmountApproval(string, func(DiskInfo) Dissent) (CancelFunc, error) {
	return nil, ErrUnsupported
}

// CurrentDisks always fails off darwin.
func (s *Session) CurrentDisks() ([]DiskInfo, error) { return nil, ErrUnsupported }

// CurrentDisks always fails off darwin.
func CurrentDisks() ([]DiskInfo, error) { return nil, ErrUnsupported }
