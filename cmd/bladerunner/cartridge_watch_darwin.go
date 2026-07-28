//go:build darwin

package main

// DiskArbitration wiring for the cartridge mount watcher. Everything that
// decides anything lives in cartridge_watch.go; this file only owns a session,
// registers the callbacks and sweeps what was already mounted.

import (
	"fmt"

	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/diskarb"
	"github.com/stuffbucket/bladerunner/internal/logging"
)

// startCartridgeWatch opens a DiskArbitration session, wires w to it and
// returns a stop function that tears the whole thing down. The returned stop is
// idempotent and safe to call from any goroutine.
//
// Order matters: the appeared watcher is registered BEFORE the catch-up sweep,
// so a cartridge mounted in between is seen by the stream rather than lost in
// the gap. Seeing it twice is harmless — the watcher's seen map collapses the
// duplicate.
func startCartridgeWatch(w *cartridgeWatcher) (stop func(), err error) {
	session, err := diskarb.NewSession()
	if err != nil {
		return nil, fmt.Errorf("open DiskArbitration session: %w", err)
	}
	// Undo whatever has been registered so far when a later step fails; the
	// session must never be left open behind a failed start.
	cancels := []diskarb.CancelFunc{}
	closeAll := func() {
		for i := len(cancels) - 1; i >= 0; i-- {
			cancels[i]()
		}
		if cerr := session.Close(); cerr != nil {
			logging.L().Debug("close DiskArbitration session", "err", cerr)
		}
	}

	appeared, err := session.WatchAppeared(w.observe)
	if err != nil {
		closeAll()
		return nil, fmt.Errorf("watch for mounted volumes: %w", err)
	}
	cancels = append(cancels, appeared)

	// Forgetting a volume on unmount is what makes re-inserting the same
	// cartridge offer again instead of being debounced forever.
	disappeared, err := session.WatchDisappeared(w.forget)
	if err != nil {
		closeAll()
		return nil, fmt.Errorf("watch for unmounted volumes: %w", err)
	}
	cancels = append(cancels, disappeared)

	// Best effort: a failed sweep costs the offer for a cartridge that was
	// already mounted, not the watch itself.
	if disks, derr := session.CurrentDisks(); derr != nil {
		logging.L().Warn("could not list mounted volumes; a cartridge mounted before now may go unnoticed", "err", derr)
	} else {
		w.catchUp(disks)
	}

	return closeAll, nil
}

// watchCartridgesForMenubar starts the mount watcher for the long-lived menubar
// process and returns a stop function (the menubar runs until it is quit, so
// the caller normally ignores it).
//
// The sink hops onto its own goroutine before doing anything user-facing: it is
// called on the DiskArbitration serial queue, where a modal prompt or a
// cartridge boot would stall every other callback on the session — including
// the unmount approvals a holder depends on.
func watchCartridgesForMenubar(p cartridgePrompter) func() {
	// The registry ROOT, not an instance state dir: this is where instances are
	// listed from and where a new holder's entry is written. It does not become
	// per-instance when several VMs are up.
	root := config.DefaultStateDir()
	w := newCartridgeWatcher(root, func(a watchAction) {
		go handleMenubarCartridge(p, a)
	})
	stop, err := startCartridgeWatch(w)
	if err != nil {
		// Not fatal: the menubar's job is the VM, and mount detection is an
		// extra. Say so once, loudly enough to explain the silence later.
		logging.L().Warn("cartridge insertion will not be detected", "err", err)
		return func() {}
	}
	logging.L().Debug("watching for cartridge insertions", "state_dir", root)
	return stop
}

// handleMenubarCartridge is the menubar's reaction to one decided volume: warn
// about a cartridge that cannot be booted, offer to boot one that can.
func handleMenubarCartridge(p cartridgePrompter, a watchAction) {
	switch a.Verdict {
	case verdictWarn:
		p.warnCartridge(a.Name, a.Reason)
	case verdictOffer:
		if !p.confirmBootCartridge(a.Name, a.SourcePath) {
			logging.L().Debug("cartridge boot declined", "name", a.Name)
			return
		}
		pid, err := bootDetectedCartridge(a)
		if err != nil {
			logging.L().Error("boot detected cartridge", "name", a.Name, "err", err)
			p.warnCartridge(a.Name, err.Error())
			return
		}
		logging.L().Info("booting detected cartridge", "name", a.Name, "pid", pid)
		p.announceCartridgeBoot(a.Name)
	case verdictIgnore, verdictBooted, verdictDeclined, verdictFailed:
		// Never delivered to a sink (ignore) or produced by this function.
	}
}
