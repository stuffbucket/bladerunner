//go:build darwin

package vmhost

import (
	"github.com/stuffbucket/bladerunner/internal/instance"
	"github.com/stuffbucket/bladerunner/internal/logging"
)

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
//
// Every failure here is a warning, never fatal. Losing the veto costs the
// user a safety net; refusing to boot because DiskArbitration was unavailable
// would cost them the VM. Each bail-out therefore returns nil — and records an
// UnprotectedReason, so "this cartridge is running unprotected" is a fact the
// Host carries rather than a line that scrolled past in a log.
func (h *Host) startUnmountWatch() error {
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

	session, err := h.openUnmountSession()
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

// stopUnmountWatch unregisters the approval callback and closes the session.
//
// Teardown order puts this immediately before the cartridge is detached, which
// is what it has to be: our own hdiutil detach is an unmount like any other and
// would otherwise be handed to our own callback.
func (h *Host) stopUnmountWatch() error {
	if h.unmountCancel == nil {
		return nil
	}
	cancel := h.unmountCancel
	h.unmountCancel = nil
	return cancel()
}
