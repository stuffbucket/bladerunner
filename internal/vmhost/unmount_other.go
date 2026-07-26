//go:build !darwin

package vmhost

import "github.com/stuffbucket/bladerunner/internal/logging"

// startUnmountWatch is a no-op off darwin: DiskArbitration is a macOS
// framework, and there is no portable equivalent that can be consulted before a
// volume is unmounted.
//
// It returns nil rather than diskarb.ErrUnsupported on purpose. This is a
// lifecycle step, so a non-nil error would abort the whole start; the veto is a
// safety net, and a Host that cannot have one must still run. Everything else
// about the shutdown path — the wait-for-stopped drain, the reverse-order
// teardown, the registry retraction — is platform-independent and unchanged.
//
// It still records UnprotectedUnsupported, so "unprotected" is reported the
// same way here as it is for a darwin bail-out; only the log level differs,
// because off darwin this is expected rather than a degradation.
func (h *Host) startUnmountWatch() error {
	if h.spec.CartridgePath == "" {
		return nil
	}
	h.setUnmountProtection(UnprotectedUnsupported)
	logging.L().Debug("unmount protection is macOS-only; continuing without it")
	return nil
}

// stopUnmountWatch is a no-op off darwin; nothing was ever registered.
func (h *Host) stopUnmountWatch() error { return nil }
