//go:build !darwin

package vmhost

import (
	"github.com/stuffbucket/bladerunner/internal/instance"
	"github.com/stuffbucket/bladerunner/internal/logging"
)

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
// because off darwin this is expected rather than a degradation (which is also
// why instance.Protection.Degraded is false for it). The test for "is there
// anything to protect" is the instance KIND, exactly as on darwin: a cartridge
// adopted from an already-open image carries no CartridgePath, and gating on
// that would have left the one instance that most needs the report silent.
func (h *Host) startUnmountWatch() error {
	if h.spec.Kind != instance.KindCartridge {
		return nil // nothing to protect; UnprotectedNotRecorded stands
	}
	h.setUnmountProtection(UnprotectedUnsupported)
	logging.L().Debug("unmount protection is macOS-only; continuing without it")
	return nil
}
