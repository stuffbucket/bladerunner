//go:build darwin

package vmhost

import (
	"fmt"

	"github.com/stuffbucket/bladerunner/internal/diskarb"
	"github.com/stuffbucket/bladerunner/internal/instance"
	"github.com/stuffbucket/bladerunner/internal/logging"
)

// unmountSession is the part of a *diskarb.Session the unmount veto uses.
//
// It is an interface so the two ways registration can fail — the session, then
// the watcher — are reachable from a test with no DiskArbitration anywhere.
// Both are bail-outs that disable protection silently, which is the failure
// mode this whole mechanism exists to prevent. It lives in the darwin file
// because that is the only place a session is ever opened.
type unmountSession interface {
	// WatchUnmountApproval registers fn for disks matching bsdName.
	WatchUnmountApproval(bsdName string, fn func(diskarb.DiskInfo) diskarb.Dissent) (diskarb.CancelFunc, error)
	// Close releases the session.
	Close() error
}

// openDiskArbSession is the production session constructor: a real
// DiskArbitration session, wrapped so the failure names what could not be done.
func openDiskArbSession() (unmountSession, error) {
	session, err := diskarb.NewSession()
	if err != nil {
		return nil, fmt.Errorf("open DiskArbitration session: %w", err)
	}
	return session, nil
}

// startUnmountWatch registers the DiskArbitration unmount-approval callback for
// the cartridge's own BSD device node, so ejecting the volume in Finder drains
// the VM instead of racing it. See Host.onUnmountApproval for the callback
// contract and for the honest limits of what a dissenter can promise.
//
// It runs directly after the cartridge is attached — earlier than the VM, on
// purpose: the guest disk, the EFI variables and the cloud-init image are all
// materialized inside the mount during startup, and an eject during that window
// is exactly as damaging as one during a running guest. Before the runner
// exists a veto still drains, which resolves to "release Run and tear down".
func (h *Host) startUnmountWatch() error {
	return h.watchUnmount(openDiskArbSession)
}

// watchUnmount is startUnmountWatch with the session constructor injected. The
// seam is a parameter rather than a field on Host because it is meaningless off
// darwin — a Host struct shared by every platform should not carry a hook only
// one of them can use — and because it is what makes the bail-outs below
// reachable from a test with no DiskArbitration anywhere.
//
// Every failure here is a warning, never fatal. Losing the veto costs the
// user a safety net; refusing to boot because DiskArbitration was unavailable
// would cost them the VM. Each bail-out therefore returns nil — and records an
// UnprotectedReason, so "this cartridge is running unprotected" is a fact the
// Host carries rather than a line that scrolled past in a log.
func (h *Host) watchUnmount(newSession func() (unmountSession, error)) error {
	if h.spec.Kind != instance.KindCartridge {
		return nil // nothing to protect; UnprotectedNone stands
	}
	if h.cartridge == nil {
		return h.unprotect(UnprotectedNoCartridge)
	}
	devNode := h.cartridge.Mount.DevNode
	if devNode == "" {
		return h.unprotect(UnprotectedNoDevNode)
	}
	// DiskArbitration matches the watcher's filter against DiskInfo.BSDName —
	// the BARE name, never a path — so the recorded "/dev/diskNsM" has to be
	// reduced first or the filter matches nothing and every eject is approved.
	// A node that reduces to nothing must not be registered either: the empty
	// filter matches every disk on the machine.
	bsdName := h.unmountFilter()
	if bsdName == "" {
		return h.unprotect(UnprotectedUnreadableDevNode, "dev_node", devNode)
	}

	session, err := newSession()
	if err != nil {
		return h.unprotect(UnprotectedNoSession, "error", err)
	}
	cancel, err := session.WatchUnmountApproval(bsdName, h.onUnmountApproval)
	if err != nil {
		_ = session.Close()
		return h.unprotect(UnprotectedWatchFailed, "bsd_name", bsdName, "error", err)
	}

	h.unmountCancel = func() error {
		cancel()
		return session.Close()
	}
	h.setUnmountProtection(UnprotectedNone)
	logging.L().Info("watching unmount requests for the cartridge",
		"bsd_name", bsdName, "dev_node", devNode)
	return nil
}

// unprotect records why the veto is off, says so once at Warn, and returns nil.
//
// Returning nil is the fail-open contract: startUnmountWatch is a lifecycle
// step, and a non-nil error would abort the whole start over a missing safety
// net. args are appended to the log line as key/value pairs. Off darwin the
// only reason is UnprotectedUnsupported, which is expected rather than a
// degradation and is recorded there at Debug instead.
func (h *Host) unprotect(why UnprotectedReason, args ...any) error {
	h.setUnmountProtection(why)
	logging.L().Warn(string(why)+"; unmount protection is off", args...)
	return nil
}
