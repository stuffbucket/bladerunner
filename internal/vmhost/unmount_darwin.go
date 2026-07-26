//go:build darwin

package vmhost

import (
	"github.com/stuffbucket/bladerunner/internal/diskarb"
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
// would cost them the VM.
func (h *Host) startUnmountWatch() error {
	if h.spec.Kind != instance.KindCartridge {
		return nil
	}
	if h.cartridge == nil {
		logging.L().Warn("no cartridge attached; unmount protection is off")
		return nil
	}
	devNode := h.cartridge.Mount.DevNode
	if devNode == "" {
		logging.L().Warn("no cartridge device node; unmount protection is off")
		return nil
	}

	session, err := diskarb.NewSession()
	if err != nil {
		logging.L().Warn("DiskArbitration unavailable; unmount protection is off", "error", err)
		return nil
	}
	cancel, err := session.WatchUnmountApproval(devNode, h.onUnmountApproval)
	if err != nil {
		_ = session.Close()
		logging.L().Warn("could not watch unmount approval; unmount protection is off",
			"dev_node", devNode, "error", err)
		return nil
	}

	h.unmountCancel = func() error {
		cancel()
		return session.Close()
	}
	logging.L().Info("watching unmount requests for the cartridge", "dev_node", devNode)
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
