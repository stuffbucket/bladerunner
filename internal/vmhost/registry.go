package vmhost

import (
	"fmt"
	"sync"

	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/instance"
	"github.com/stuffbucket/bladerunner/internal/logging"
)

// RegistryRoot returns the directory whose instances/ subdirectory holds the
// registry every holder publishes into.
//
// It is deliberately the HOST's default state dir and not the instance's own
// state dir. A cartridge instance is rooted at its mountpoint — which now lives
// under /Volumes and can be anywhere — so an entry written "next to the
// instance" would travel with the cartridge and be invisible to a manager
// looking for running VMs. One fixed, host-local registry is what makes
// `br ls`, mount detection and eject able to find holders they did not start.
//
// BLADERUNNER_STATE_DIR (or XDG_STATE_HOME) relocates it, which is what keeps
// tests and parallel installs from sharing one registry.
func RegistryRoot() string { return config.DefaultStateDir() }

// registry publishes and retracts one instance's registry entry.
//
// Publication is idempotent and change-gated: publish writes only when the
// entry actually differs from the last one written, so the Host can call it
// after every step that might have changed a port or a mountpoint without
// rewriting the file each time.
//
// Every failure is reported to the caller but is non-fatal by design: the
// registry is an optimization for OTHER processes, and a VM that is running
// perfectly well must not be torn down because a JSON file could not be
// written. The Host logs and continues.
type registry struct {
	root string

	mu        sync.Mutex
	name      string
	published bool
	last      instance.Entry
}

// newRegistry returns a registry rooted at root. A zero root disables
// publication entirely (every method becomes a no-op), which is what a caller
// with no host state directory wants.
func newRegistry(root string) *registry { return &registry{root: root} }

// publish writes e, unless it is byte-identical to the entry last written.
//
// The entry name must satisfy instance.ValidName. It usually does — it is
// derived from the instance's directory name — but a Finder mount-collision
// suffix ("bladerunner-demo 1") can produce one that does not, and that must
// not fail a boot. Such an instance simply goes unregistered, with a warning.
func (r *registry) publish(e instance.Entry) error {
	if r == nil || r.root == "" {
		return nil
	}
	if err := instance.ValidName(e.Name); err != nil {
		return fmt.Errorf("instance is not registrable: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.published && r.last == e {
		return nil
	}
	if err := instance.Write(r.root, e); err != nil {
		return err
	}
	r.name, r.last, r.published = e.Name, e, true
	return nil
}

// remove retracts the entry this registry published. It is idempotent and a
// no-op if nothing was ever published.
func (r *registry) remove() error {
	if r == nil || r.root == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.published {
		return nil
	}
	// Clear the state first: a failed removal must not leave the registry
	// believing it still owns an entry it may no longer be able to rewrite.
	name := r.name
	r.published, r.name, r.last = false, "", instance.Entry{}
	return instance.Remove(r.root, name)
}

// prune garbage-collects entries left behind by holders that died without
// retracting theirs. It is what makes the registry crash-tolerant: a stale
// entry is not a permanent lie, it is one that the next holder (or any reader
// calling instance.Prune) cleans up.
//
// instance.Alive is deliberately conservative — a live PID or a socket file is
// enough to keep an entry — so this can never unregister a running VM.
func (r *registry) prune() {
	if r == nil || r.root == "" {
		return
	}
	removed, err := instance.Prune(r.root)
	if err != nil {
		logging.L().Warn("could not prune the instance registry", "root", r.root, "error", err)
		return
	}
	if len(removed) > 0 {
		logging.L().Info("pruned dead instance registry entries", "names", removed)
	}
}

// startRegistry publishes this instance's entry. It runs immediately after the
// control socket is bound, so the moment an instance is addressable it is also
// discoverable, and it prunes dead entries first so a crashed predecessor does
// not linger.
//
// Publication failure is logged, never fatal: see registry.
func (h *Host) startRegistry() error {
	h.reg = newRegistry(RegistryRoot())
	h.reg.prune()
	h.republishRegistry()
	return nil
}

// stopRegistry retracts the entry on the way out. Teardown runs it in reverse
// order, before the control socket closes, so a reader never sees an entry
// whose socket has already gone.
func (h *Host) stopRegistry() error {
	return h.reg.remove()
}

// republishRegistry re-reads Info and republishes it if anything changed —
// after the ports are reserved, and again once the VM is up and the mountpoint
// and ssh config are final.
//
// It is only ever called from the goroutine driving Run, so the only writer it
// can race is a concurrent `br config set` arriving on the control socket. The
// Info snapshot is therefore taken under the config router's lock, exactly as
// every other config access is. (lockedConfig is not re-entrant, so callers
// must not already hold it — none do.)
func (h *Host) republishRegistry() {
	var entry instance.Entry
	h.lockedConfig(func() { entry = h.Info() })
	if err := h.reg.publish(entry); err != nil {
		logging.L().Warn("could not publish the instance registry entry", "error", err)
	}
}
