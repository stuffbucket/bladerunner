// Package vmhost owns the lifecycle of exactly one bladerunner VM instance:
// the cartridge mount it boots from, the control socket, the reserved host
// ports, the OIDC and SNTP side services, the VM itself, and the web-UI proxy.
//
// It exists so that lifecycle is importable. It used to live in `package main`
// inside runStart, which meant a standalone holder process — the whole point of
// the cartridge runtime — could not reach it. Accordingly this package imports
// no cobra, no systray, and nothing Cocoa: a future `cmd/br-vmd` can import
// vmhost and (almost) nothing else. Everything user-facing is delegated to an
// Observer the front end installs, so the CLI keeps its banners, its boot
// board, and its --json report without vmhost knowing they exist.
//
// # Ownership and ordering
//
// A Host runs a fixed, named list of steps (see the Step* constants). Teardown
// is the exact reverse of the order the steps started in, it skips steps that
// never started, and it is idempotent. That property is the whole reason the
// steps are a data structure rather than a run of deferred calls: it can be
// tested without a VM.
//
// # Threading
//
// main.go pins the process to its original OS thread (runtime.LockOSThread in
// init) because Virtualization.framework requires the GUI event loop to own the
// main thread. Consequently:
//
//   - Run MUST be called from the main goroutine on the locked main thread. In
//     GUI mode it never returns until the VM window closes, because it blocks
//     inside vz.StartGraphicApplication.
//   - New, Drain, Info, Runner and Cartridge are safe from any goroutine. Drain
//     in particular is called from a control-socket handler goroutine and from
//     a DiskArbitration dispatch queue.
//
// Two Hosts can be constructed in one process — there is no package-level
// mutable state — but only one can be Run today, because only one goroutine can
// hold the main thread. That restriction is a property of GUI mode, not of this
// type, and it is why the design puts each instance in its own holder process.
package vmhost

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stuffbucket/bladerunner/internal/bootstage"
	"github.com/stuffbucket/bladerunner/internal/cartridge"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/control"
	"github.com/stuffbucket/bladerunner/internal/diskarb"
	"github.com/stuffbucket/bladerunner/internal/instance"
	"github.com/stuffbucket/bladerunner/internal/logging"
	"github.com/stuffbucket/bladerunner/internal/oidc"
	"github.com/stuffbucket/bladerunner/internal/portalloc"
	"github.com/stuffbucket/bladerunner/internal/ssh"
	"github.com/stuffbucket/bladerunner/internal/timesource"
	"github.com/stuffbucket/bladerunner/internal/vm"
	"github.com/stuffbucket/bladerunner/internal/webproxy"
)

// Step names. They label the ordered start/stop pairs a Host runs, and they are
// what an Observer sees when a teardown step fails.
const (
	// StepCartridge attaches (or adopts) the cartridge image. It runs first
	// because every other path — including the control socket — lives inside
	// the mount, and it is torn down last for the same reason.
	StepCartridge = "cartridge"
	// StepUnmountVeto registers the DiskArbitration unmount-approval callback
	// for the cartridge's own device node. It runs directly after the mount
	// exists and is unregistered directly before the mount goes away.
	StepUnmountVeto = "unmountveto"
	// StepControl resolves the base config and binds the control socket.
	StepControl = "control"
	// StepServe starts answering on the bound control socket.
	StepServe = "serve"
	// StepRegistry publishes this instance's registry entry, so a process that
	// did not start the VM can find it.
	StepRegistry = "registry"
	// StepConfig overlays Settings, the disk manifest, the CLI overrides and
	// the cartridge onto the config, then brings logging up.
	StepConfig = "config"
	// StepPorts reserves this instance's host loopback ports as bound listeners.
	StepPorts = "ports"
	// StepSSHKeys materializes the host SSH key pair.
	StepSSHKeys = "sshkeys"
	// StepOIDC starts the local OIDC provider.
	StepOIDC = "oidc"
	// StepNTP starts the host pseudo-NTP responder.
	StepNTP = "ntp"
	// StepRunner constructs the VM runner (but does not start the VM).
	StepRunner = "runner"
	// StepBootStage publishes the coarse boot phase for the menubar splash.
	StepBootStage = "bootstage"
	// StepVM starts the VM and wires everything that needs a running guest.
	StepVM = "vm"
	// StepWebProxy starts the host-side web-UI TLS proxy.
	StepWebProxy = "webproxy"
)

// ErrNotStarted is returned by Drain (and reported by the control-plane save
// and eject handlers) before the VM runner exists.
var ErrNotStarted = errors.New("VM is not started yet")

// ErrAlreadyRunning is returned when another process already holds this
// instance's control socket.
var ErrAlreadyRunning = errors.New("VM is already running (use 'br stop' first)")

// ErrStopRequested is the CAUSE recorded on the run context when the instance
// is released from inside this process — the control-plane stop, an eject, or
// an unmount-triggered drain — rather than by the caller's own context.
//
// It is never returned to anyone. It exists so a wait that ends early can say
// which of the two things happened, because "context canceled" alone reads
// exactly like a boot timeout in a log and is the reason a cut-short boot took
// a whole VM run to diagnose. See cancelReason.
var ErrStopRequested = errors.New("stop requested through the control plane")

// errOIDCDisabled signals that the OIDC provider was intentionally skipped
// (e.g. LocalOIDCPort=0). Callers should treat it as a benign no-op.
var errOIDCDisabled = errors.New("oidc disabled (LocalOIDCPort=0)")

// guestProbeTimeout bounds the guest-liveness probe `br status` runs through
// the control socket.
const guestProbeTimeout = 2 * time.Second

// DefaultDrainTimeout is the budget Drain gives the guest to power itself off
// when neither the caller nor the Spec supplies one. It mirrors the vm
// package's own default, which is declared in a darwin-only file and so cannot
// be referenced from portable code (follow-up: lift it to a shared file).
const DefaultDrainTimeout = 60 * time.Second

// unmountDrainGrace is the slack added to the drain budget when bounding the
// context of an unmount-triggered drain, so the context deadline never fires
// before the drain's own escalation has had a chance to run.
const unmountDrainGrace = 15 * time.Second

// Observer receives the lifecycle notifications a front end renders. Every
// method is called from the goroutine driving Run, in the documented order, so
// implementations do not need to be concurrency-safe among themselves.
type Observer interface {
	// Resolved fires once the config is final and the VM runner exists, before
	// any progress sink is attached. This is where the CLI prints its banner.
	Resolved(cfg *config.Config)
	// Progress fires immediately before the VM starts. A non-nil return is
	// added to the boot progress fan-out (the CLI's TTY boot board); ctx is
	// canceled when the host shuts down.
	Progress(ctx context.Context, cfg *config.Config) vm.Progress
	// Failed fires when the VM itself failed to start, before teardown.
	Failed(err error)
	// Started fires in GUI mode once the VM is running: boot success is not yet
	// known, because the window has to open before the readiness wait can run.
	Started(cfg *config.Config, endpoint string)
	// Ready fires in headless mode once the readiness wait has finished;
	// bootErr is nil when the guest reached the Incus-ready state.
	Ready(cfg *config.Config, endpoint string, bootErr error)
	// Waiting fires when the host is fully up and about to block — on the GUI
	// event loop when gui is true, on context cancellation otherwise.
	Waiting(gui bool)
	// Stopping fires once the block has been released and teardown is next.
	Stopping()
	// TeardownWarning fires when a teardown step failed. Teardown continues
	// regardless; the front end decides what is worth telling the user.
	TeardownWarning(step string, err error)
}

// NopObserver is an Observer that renders nothing. It is the default, and it is
// what a headless holder process wants.
type NopObserver struct{}

// Resolved implements Observer.
func (NopObserver) Resolved(*config.Config) {}

// Progress implements Observer.
func (NopObserver) Progress(context.Context, *config.Config) vm.Progress { return nil }

// Failed implements Observer.
func (NopObserver) Failed(error) {}

// Started implements Observer.
func (NopObserver) Started(*config.Config, string) {}

// Ready implements Observer.
func (NopObserver) Ready(*config.Config, string, error) {}

// Waiting implements Observer.
func (NopObserver) Waiting(bool) {}

// Stopping implements Observer.
func (NopObserver) Stopping() {}

// TeardownWarning implements Observer.
func (NopObserver) TeardownWarning(string, error) {}

// step is one named unit of the host lifecycle: something to bring up and the
// matching thing to take back down. A start that fails must leave nothing
// behind, which is why a failed step is never pushed onto the stack.
type step struct {
	name  string
	start func(context.Context) error
	stop  func() error
}

// noCtx adapts a start closure that has nothing to cancel to the step
// signature. Most steps are synchronous setup; only the ones that hand a
// context to something long-lived (the control listener, the OIDC server, the
// VM boot) actually take one.
func noCtx(fn func() error) func(context.Context) error {
	return func(context.Context) error { return fn() }
}

// stepStack records which steps actually started, so teardown can unwind them
// in exact reverse order — and only them.
type stepStack struct {
	mu      sync.Mutex
	started []step
}

// run starts each step in order. The first failure unwinds every step that had
// already started (in reverse) and returns that step's error unchanged.
func (s *stepStack) run(ctx context.Context, steps []step, onStopErr func(string, error)) error {
	for _, st := range steps {
		if st.start != nil {
			if err := st.start(ctx); err != nil {
				s.teardown(onStopErr)
				return err
			}
		}
		s.mu.Lock()
		s.started = append(s.started, st)
		s.mu.Unlock()
	}
	return nil
}

// teardown stops every started step in exact reverse order. It is idempotent:
// the stack is drained under the lock before anything is stopped, so a second
// call (from a deferred cleanup after run already unwound) does nothing.
func (s *stepStack) teardown(onStopErr func(string, error)) {
	s.mu.Lock()
	pending := s.started
	s.started = nil
	s.mu.Unlock()

	for i := len(pending) - 1; i >= 0; i-- {
		st := pending[i]
		if st.stop == nil {
			continue
		}
		if err := st.stop(); err != nil && onStopErr != nil {
			onStopErr(st.name, err)
		}
	}
}

// Host owns one instance's lifecycle. Build it with New, drive it with Run.
type Host struct {
	spec Spec
	obs  Observer

	stack     stepStack
	startedAt time.Time

	cfg       *config.Config
	cfgRouter *control.ConfigRouter
	cartridge *cartridge.Opened
	// adopted records that the cartridge was opened by the caller and handed
	// over; the Host still closes it, but never re-opens it.
	adopted bool

	ctrl     *control.LocalController
	listener *control.Listener

	oidc     *oidc.Provider
	ntp      *timesource.Responder
	webProxy *webproxy.Proxy
	bootPub  *bootStagePublisher
	reg      *registry

	// unmountCancel unregisters the DiskArbitration unmount-approval watcher
	// and closes its session. It is nil when no watcher was registered — a
	// non-cartridge instance, a platform without DiskArbitration, or a failed
	// registration (which is warned about, never fatal).
	unmountCancel func() error
	// unprotected records whether this instance's cartridge is covered by the
	// unmount veto, and when it is not, why — UnprotectedNone when it is armed,
	// UnprotectedNotRecorded when nothing decided (a non-cartridge instance).
	// Guarded by mu: it is written on the goroutine driving Run and read by
	// whoever reports instance state.
	unprotected UnprotectedReason

	// drainOnce guarantees that however many unmount-approval callbacks fire —
	// Finder retries, and DiskArbitration delivers one per slice — exactly one
	// drain is started.
	drainOnce sync.Once
	// drainKick is what a vetoed unmount runs, on its own goroutine, to spin
	// the guest down. It is a field only so tests can substitute a fake; nil
	// means drainForUnmount.
	drainKick func()
	// draining records that a drain is in flight, and guestStopped that the
	// guest has reached a stopped state (or that teardown has begun, which
	// implies it). Together they are the whole input to the unmount-approval
	// decision, and they are atomics because that decision is made on the
	// DiskArbitration dispatch queue.
	draining     atomic.Bool
	guestStopped atomic.Bool

	// hostPublicKey is the host's own SSH public key, kept separately from the
	// config because SetSSHKeys does not overwrite a key the config already
	// carried and OIDC bootstraps from the key this process actually holds.
	hostPublicKey string

	endpoint string

	mu     sync.Mutex
	runner *vm.Runner
	cancel context.CancelFunc

	// stepsFn, waitReady and stopVM are TEST SEAMS. None is ever set outside a
	// test — New leaves all three nil and every production path resolves to
	// h.steps, h.waitForGuestReady and the live runner — so do not delete them
	// as unused (AGENTS.md section 9 point 4: a name only a test needs).
	//
	// They exist because Run and block are the two functions in this package
	// that no unit test could reach: every one of the fourteen real steps
	// attaches a disk image, binds a socket or starts a VM, and the readiness
	// wait dials a guest through a live vm.Runner that only exists after
	// Virtualization.framework has booted one from a signed binary. That left
	// Run's OWN guarantees — the recorded cancel cause, the startedAt stamp,
	// teardown on every exit path including a step failure — verifiable only by
	// running a VM by hand. Substituting the step list and the readiness wait
	// makes those guarantees testable without booting anything; nothing else
	// about Run is faked.
	//
	// stopVM is the same idea for the teardown side: the drain budget stopRunner
	// hands the guest is only observable through a live *vm.Runner, so without
	// this seam "the Spec's DrainTimeout is the budget the guest actually gets"
	// could not be asserted without booting a VM. It substitutes the stop call
	// alone; the budget it receives is resolved by production code.
	stepsFn   func() []step
	waitReady func(context.Context) error
	stopVM    func(context.Context, time.Duration) error
}

// lifecycleSteps resolves the ordered lifecycle Run drives: the real one from
// steps, unless a test installed a substitute.
func (h *Host) lifecycleSteps() []step {
	if h.stepsFn != nil {
		return h.stepsFn()
	}
	return h.steps()
}

// guestReady resolves the readiness wait block performs: the real one from
// waitForGuestReady, unless a test installed a substitute.
func (h *Host) guestReady(ctx context.Context) error {
	if h.waitReady != nil {
		return h.waitReady(ctx)
	}
	return h.waitForGuestReady(ctx)
}

// stopGuest resolves the guest teardown stopRunner performs: the live runner,
// unless a test installed a substitute. A Host with no runner has nothing to
// stop, which is not an error — a boot can fail before the VM is constructed.
//
// The runner's own stop error is deliberately dropped rather than returned:
// teardown must continue to the cartridge detach whatever the VMM did, and the
// runner has already logged the outcome.
func (h *Host) stopGuest(ctx context.Context, budget time.Duration) error {
	if h.stopVM != nil {
		return h.stopVM(ctx, budget)
	}
	if r := h.activeRunner(); r != nil {
		_ = r.StopWithTimeout(ctx, budget)
	}
	return nil
}

// New validates spec and returns a Host ready to Run. It performs no I/O and
// starts nothing, so constructing a Host is always cheap and always reversible.
func New(spec Spec) (*Host, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return &Host{spec: spec, obs: NopObserver{}}, nil
}

// SetObserver installs the front end's render hooks. Passing nil restores the
// silent default. Call it before Run.
func (h *Host) SetObserver(obs Observer) {
	if obs == nil {
		obs = NopObserver{}
	}
	h.obs = obs
}

// AdoptCartridge hands the Host an already-open cartridge to own, instead of
// having it open Spec.CartridgePath itself. The Host closes it during teardown
// either way. Passing nil is a no-op, so a caller can hand over whatever it has
// without branching. Call it before Run.
func (h *Host) AdoptCartridge(opened *cartridge.Opened) {
	if opened == nil {
		return
	}
	h.cartridge = opened
	h.adopted = true
}

// Runner and Cartridge are the package boundary, not leftovers: the two
// resources a front end legitimately has to reach past the Host to work with.
// A dead-code sweep sees few callers of either because most consumers live in
// other repos' shoes — cmd/bladerunner and the menubar — so keep them.
//
// The resolved config and the published endpoint are deliberately NOT exposed:
// both are delivered to the front end through Observer.Ready, which is the one
// point at which they are complete.

// Runner returns the VM runner, or nil before the VM has been constructed.
func (h *Host) Runner() *vm.Runner { return h.activeRunner() }

// Cartridge returns the cartridge this instance booted from, or nil.
func (h *Host) Cartridge() *cartridge.Opened { return h.cartridge }

// instanceName is the one name this instance is known by: the registry key, the
// ssh config fragment, and therefore what `--instance <name>` and `br eject
// <name>` address it as.
//
// The cartridge's OWN name is preferred over the state directory's basename,
// and that ordering is the fix for a real bug. A cartridge is mounted browsably
// now, so macOS roots its state dir at /Volumes/bladerunner-<name> and
// config.InstanceName — which is just filepath.Base — yields
// "bladerunner-demo". Everything that addresses the instance by name then has
// to know about a prefix the user never typed. Opened.Name already carries the
// user-facing name ("demo"); this consults it.
//
// A cartridge name that is not a legal instance name falls through to the
// derived one rather than producing an entry the registry must refuse.
func (h *Host) instanceName() string {
	if h.spec.Name != "" {
		return h.spec.Name
	}
	if h.cartridge != nil && instance.ValidName(h.cartridge.Name) == nil {
		return h.cartridge.Name
	}
	if h.cfg != nil {
		return h.cfg.InstanceName()
	}
	return ""
}

// Info describes the running instance in the form the registry records. It is
// safe to call at any point; fields that are not known yet are zero.
func (h *Host) Info() instance.Entry {
	e := instance.Entry{
		Name:              h.instanceName(),
		Kind:              h.spec.Kind,
		SourcePath:        h.spec.CartridgePath,
		PID:               os.Getpid(),
		ProtocolVersion:   control.ProtocolVersion,
		BinaryVersion:     h.spec.BinaryVersion,
		StartedAt:         h.runStartedAt(),
		UnmountProtection: h.UnmountProtection(),
	}
	if h.cfg != nil {
		e.StateDir = h.cfg.VMDir
		e.GUI = h.cfg.GUI
		p := h.cfg.Ports()
		e.Ports = instance.Ports{SSH: p.SSH, API: p.API, Web: p.Web, OIDC: p.OIDC, NTP: p.NTP}
	}
	if h.cartridge != nil {
		e.WorkingCopy = h.cartridge.WorkingCopy
		e.DevNode = h.cartridge.Mount.DevNode
		e.Mountpoint = h.cartridge.Mountpoint()
		if e.SourcePath == "" {
			e.SourcePath = h.cartridge.SourcePath
		}
	}
	return e
}

// Run brings the instance up and blocks until ctx is canceled, the guest is
// ejected, or (in GUI mode) the VM window closes. Teardown is always performed
// before it returns, in exact reverse order.
//
// It must be called on the main thread: GUI mode hands that thread to
// Virtualization.framework's event loop and never gives it back.
func (h *Host) Run(ctx context.Context) error {
	// WithCancelCause, not WithCancel: whoever releases the run records WHY, so
	// a readiness wait that ends early can name it instead of reporting a bare
	// "context canceled" that is indistinguishable from its own budget expiring.
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	// startedAt is published under the same lock as cancel. Info() is
	// documented as safe from any goroutine, and it reads this field; every
	// goroutine that calls Info in production today is created BY A STEP, so
	// the write below happens-before every read by accident of ordering. A
	// caller that holds a *Host and polls Info() from a goroutine started
	// before Run has no such edge, and the race detector proves it.
	h.mu.Lock()
	h.cancel = func() { cancel(ErrStopRequested) }
	h.startedAt = time.Now()
	h.mu.Unlock()

	defer h.teardown()

	if err := h.stack.run(ctx, h.lifecycleSteps(), h.onStopErr); err != nil {
		return err
	}
	return h.block(ctx)
}

// teardown unwinds every started step. It marks the guest stopped first, so an
// unmount-approval callback that fires while the steps are unwinding approves
// (there is nothing left to protect) instead of vetoing our own detach.
func (h *Host) teardown() {
	h.guestStopped.Store(true)
	h.stack.teardown(h.onStopErr)
}

// Drain performs the orderly spin-down: an ACPI power request, a genuine wait
// for the guest to reach the stopped state, and an explicit escalation only if
// that budget expires. On success it releases Run, whose teardown then detaches
// the cartridge with the VMM already gone. A timeout <= 0 uses the Spec's
// DrainTimeout, or the vm package default.
//
// Safe to call from any goroutine.
func (h *Host) Drain(ctx context.Context, timeout time.Duration) error {
	return h.drain(ctx, timeout, false)
}

// drain is the shared body of Drain and the control-plane eject handler; force
// skips straight to the destructive stop.
//
// It maintains the two flags the unmount-approval decision reads: draining for
// the whole call, and guestStopped once the guest is genuinely down (including
// the "there was never a VM" case, where there is nothing left to protect).
func (h *Host) drain(ctx context.Context, timeout time.Duration, force bool) error {
	r := h.activeRunner()
	if r == nil {
		h.guestStopped.Store(true)
		return ErrNotStarted
	}
	if timeout <= 0 {
		timeout = h.spec.DrainTimeout
	}
	if timeout <= 0 {
		timeout = DefaultDrainTimeout
	}

	h.draining.Store(true)
	defer h.draining.Store(false)

	if err := r.Eject(ctx, timeout, force); err != nil {
		return err
	}
	h.guestStopped.Store(true)
	h.stop()
	return nil
}

// unmountDenyReason is the string DiskArbitration hands back to whoever asked
// for the unmount; Finder shows it verbatim in its "could not eject" dialog. It
// is shared with the bootstage detail so the menubar notice and the Finder
// dialog say exactly the same thing.
const unmountDenyReason = bootstage.DetailUnmountRequested

// UnprotectedReason says why a cartridge instance is running WITHOUT the
// DiskArbitration unmount veto, or that it is armed.
//
// The veto fails OPEN on purpose — a safety net that cannot be registered must
// never stop the VM from starting — which is exactly why the outcome has to be
// recorded rather than merely logged at Warn. This shipped once as a silent
// loss of protection: the watcher was armed with a filter that could not match,
// so every eject of a running cartridge was approved and nobody could tell.
// A value here is the difference between "protected" and "unprotected, for this
// stated reason", and it is what lets `br instances` / `br status` say so.
//
// It is an ALIAS of instance.Protection, not a second vocabulary. The value is
// published in this instance's registry entry (Info sets
// instance.Entry.UnmountProtection), which is how it reaches a CLI running in
// another process, and the registry record's format is owned by
// internal/instance. Its Reason method supplies the human clause the warning
// below prints, so the log line and anything a front end renders cannot drift.
type UnprotectedReason = instance.Protection

// The outcomes the veto can have. Each of the failures corresponds to exactly
// one bail-out in startUnmountWatch; every one of them returns nil, so the only
// way to observe which fired is this value.
//
// These names are kept as the vmhost spelling of the instance.Protection
// constants: this package is where each one is DECIDED, and a reader of
// startUnmountWatch should not have to change packages to see what it can
// record.
const (
	// UnprotectedNone means the veto is armed. Note that it is NOT the zero
	// value: an instance whose outcome was never recorded — a non-cartridge
	// instance, or one started by a build that predates the record — is
	// instance.ProtectionUnrecorded, and reporting that as "protected" is the
	// lie this value exists to prevent.
	UnprotectedNone = instance.ProtectionArmed
	// UnprotectedNotRecorded is the zero value: nothing decided an outcome,
	// because there is nothing to protect.
	UnprotectedNotRecorded = instance.ProtectionUnrecorded
	// UnprotectedNoCartridge means the cartridge step produced no open mount,
	// so there is no device to watch on an instance that expected one.
	UnprotectedNoCartridge = instance.ProtectionNoCartridge
	// UnprotectedNoDevNode means the mount recorded no device node.
	UnprotectedNoDevNode = instance.ProtectionNoDevNode
	// UnprotectedUnreadableDevNode means the recorded device node does not name
	// a BSD disk, so no filter can be derived from it. Registering the empty
	// filter instead would arm the veto over every disk on the machine.
	UnprotectedUnreadableDevNode = instance.ProtectionUnreadableDevNode
	// UnprotectedNoSession means the DiskArbitration session could not be
	// opened.
	UnprotectedNoSession = instance.ProtectionNoSession
	// UnprotectedWatchFailed means the session was opened but the approval
	// watcher could not be registered on it.
	UnprotectedWatchFailed = instance.ProtectionWatchFailed
	// UnprotectedUnsupported means the platform has no DiskArbitration at all.
	UnprotectedUnsupported = instance.ProtectionUnsupported
)

// UnmountProtection reports whether this instance's cartridge is protected
// against an unmount, and when it is not, why. It is safe from any goroutine.
//
// It is what Info publishes into the registry entry, so `br instances` and
// `br status` can tell a user their cartridge can be pulled out from under a
// running guest.
func (h *Host) UnmountProtection() UnprotectedReason {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.unprotected
}

// setUnmountProtection records the veto's outcome.
func (h *Host) setUnmountProtection(why UnprotectedReason) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.unprotected = why
}

// runStartedAt reports when Run stamped this instance's start, or the zero time
// if it has not started.
//
// It exists so Info can read startedAt under h.mu without holding that lock
// across the whole of Info: h.mu is a plain Mutex, and Info already calls the
// locking UnmountProtection, so a single lock spanning both would deadlock.
func (h *Host) runStartedAt() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.startedAt
}

// The session type the veto registers on, the constructor that opens it and the
// bail-out helper that records a lost veto all live in unmount_darwin.go: they
// are DiskArbitration's, and DiskArbitration is macOS-only. What stays here is
// what both platforms share — the recorded reason, the filter, the decision, and
// the release below.

// stopUnmountWatch releases the unmount-approval registration, exactly once
// however many times it is called: teardown runs it, and a failed start may have
// run it already.
//
// It is platform-independent on purpose. Only h.unmountCancel is touched, and
// the DiskArbitration work is inside that closure, captured when the watch was
// registered. Off darwin the field is never set, so this is a no-op — but the
// invariant "stopUnmountWatch releases the cancel exactly once" then holds on
// every platform, rather than being a darwin-only promise.
func (h *Host) stopUnmountWatch() error {
	if h.unmountCancel == nil {
		return nil
	}
	cancel := h.unmountCancel
	h.unmountCancel = nil
	return cancel()
}

// unmountFilter is the BSD name the unmount-approval watcher registers for, or
// "" when there is nothing to protect: no cartridge, or a recorded device node
// that does not name a BSD disk. It is what startUnmountWatch passes to diskarb,
// kept here — portable, and away from the cgo session — so the filter can be
// asserted without a Mac.
//
// The reduction is diskarb.BSDName's, and using it is the whole of the veto's
// correctness. The watcher filter is compared against DiskInfo.BSDName, which is
// never a path, so registering the recorded "/dev/disk9s1" verbatim matched
// nothing: every approval callback returned "approve" and the drain below was
// unreachable.
//
// "" is never registered. An empty diskarb filter matches EVERY disk, so a
// device node this package cannot read must disable the veto (startUnmountWatch
// records UnprotectedUnreadableDevNode) rather than veto the whole machine.
func (h *Host) unmountFilter() string {
	if h.cartridge == nil {
		return ""
	}
	return diskarb.BSDName(h.cartridge.Mount.DevNode)
}

// unmountState is the entirety of the Host lifecycle that the unmount-approval
// decision depends on. Isolating it is what makes decideUnmount a pure function
// that can be tested exhaustively without a disk, a VM or a Mac.
type unmountState struct {
	// Stopped reports that the guest is down — it drained successfully, it was
	// never started, or teardown has begun. Nothing is writing to the volume.
	Stopped bool
	// Draining reports that a drain is already in flight, so a second one must
	// not be started.
	Draining bool
}

// unmountDecision is the answer to one unmount-approval callback: what to tell
// DiskArbitration, and whether this callback is the one that must start the
// drain.
type unmountDecision struct {
	// Dissent is returned to DiskArbitration immediately.
	Dissent diskarb.Dissent
	// StartDrain asks the caller to kick the orderly spin-down off on a
	// background goroutine. It is never true when Dissent approves.
	StartDrain bool
}

// decideUnmount answers an unmount-approval request.
//
// The rule is small and total: once the guest is stopped there is nothing left
// to protect, so approve; otherwise veto and — unless one is already running —
// start the drain. It never blocks and never touches I/O, because it is called
// on the DiskArbitration serial dispatch queue, where the requester (Finder,
// diskutil, hdiutil) is blocked until it returns.
func decideUnmount(st unmountState) unmountDecision {
	if st.Stopped {
		return unmountDecision{Dissent: diskarb.Approve()}
	}
	return unmountDecision{
		Dissent:    diskarb.Deny(unmountDenyReason),
		StartDrain: !st.Draining,
	}
}

// unmountState samples the flags decideUnmount reads. Both are atomics, so it
// is safe on the DiskArbitration queue.
func (h *Host) unmountState() unmountState {
	return unmountState{
		Stopped:  h.guestStopped.Load(),
		Draining: h.draining.Load(),
	}
}

// onUnmountApproval is the DiskArbitration unmount-approval callback.
//
// # It must return promptly and it does
//
// It runs on the diskarb session's serial dispatch queue with the requester
// blocked, so it does exactly two things: compute a decision, and (at most
// once, guarded by drainOnce) spawn the drain on a goroutine. It never waits
// for the drain, which can take the full 60-second budget. The user's eject
// fails with the reason string; when the drain finishes, the Host completes
// teardown — unregistering this very callback, detaching the cartridge and
// retracting the registry entry — so the volume goes away by itself and a
// second eject click is not usually even needed.
//
// # Honest limitation: DADissenter is ADVISORY
//
// A dissenter delays a polite unmount; it does not prevent an impolite one.
// Finder's "Force Eject", `diskutil unmount force` and
// DADiskUnmount(kDADiskUnmountOptionForce) all bypass registered dissenters
// outright, and a direct umount(2) never consults DiskArbitration at all. This
// veto therefore narrows the window in which a running VM's disk can be pulled
// out from under it; it does not close it. The real protection is the
// wait-for-stopped drain (internal/vm.drainGuest) and the cache/sync disk
// attachment, both of which hold regardless of who asked for the unmount.
func (h *Host) onUnmountApproval(disk diskarb.DiskInfo) diskarb.Dissent {
	decision := decideUnmount(h.unmountState())
	if decision.StartDrain {
		h.drainOnce.Do(func() {
			logging.L().Info("unmount requested for a running cartridge; draining the VM",
				"bsd_name", disk.BSDName, "volume", disk.VolumePath)
			h.kickUnmountDrain()
		})
	}
	return decision.Dissent
}

// kickUnmountDrain starts the drain on its own goroutine so the approval
// callback can return at once.
func (h *Host) kickUnmountDrain() {
	run := h.drainKick
	if run == nil {
		run = h.drainForUnmount
	}
	go run()
}

// drainForUnmount performs the orderly spin-down a vetoed eject asked for and
// then releases Run so teardown detaches the cartridge.
//
// The context is deliberately Background-rooted with its own budget: the drain
// must outlive the DiskArbitration callback that triggered it, and it must
// still be bounded if the guest never powers off.
func (h *Host) drainForUnmount() {
	reporter := h.shutdownReporter()
	_ = reporter.Draining(bootstage.DetailUnmountRequested)

	timeout := h.drainTimeout()
	ctx, cancel := context.WithTimeout(context.Background(), timeout+unmountDrainGrace)
	defer cancel()

	switch err := h.Drain(ctx, timeout); {
	case err == nil, errors.Is(err, ErrNotStarted):
		_ = reporter.Ejecting("")
	default:
		logging.L().Warn("drain after an unmount request failed; forcing teardown", "error", err)
		_ = reporter.Forced(bootstage.DetailForced)
	}
	// Drain releases Run itself on success. Do it unconditionally so a failed
	// or never-started drain still ends in teardown rather than a wedged host
	// holding a volume the user asked to eject.
	h.stop()
}

// drainTimeout resolves the budget an unmount-triggered drain runs under.
func (h *Host) drainTimeout() time.Duration {
	if h.spec.DrainTimeout > 0 {
		return h.spec.DrainTimeout
	}
	return DefaultDrainTimeout
}

// shutdownReporter returns the bootstage reporter for the drain/eject stages,
// or nil before the config is resolved. A nil *bootstage.Reporter is usable and
// does nothing, so callers need not branch.
func (h *Host) shutdownReporter() *bootstage.Reporter {
	if h.cfg == nil || h.cfg.VMDir == "" {
		return nil
	}
	return bootstage.NewReporter(h.cfg.VMDir)
}

// block waits for the instance to finish. In GUI mode it surrenders the main
// thread to the VM window and runs the readiness wait in the background,
// because the macOS event loop has to start immediately; headless it blocks on
// readiness first so the boot board can render through to "ready".
func (h *Host) block(ctx context.Context) error {
	if h.cfg.GUI {
		// GUI mode can't block on Incus before opening the window — the
		// macOS event loop must run on the main thread immediately. We
		// don't yet know if boot will succeed, so don't claim it did.
		h.obs.Started(h.cfg, h.endpoint)
		go func() { h.publishBootOutcome(h.guestReady(ctx)) }()

		h.obs.Waiting(true)
		if err := h.activeRunner().StartGUI(); err != nil {
			return fmt.Errorf("start gui: %w", err)
		}
	} else {
		bootErr := h.guestReady(ctx)
		h.publishBootOutcome(bootErr)
		h.obs.Ready(h.cfg, h.endpoint, bootErr)
		h.obs.Waiting(false)
		<-ctx.Done()
	}

	h.obs.Stopping()
	return nil
}

// publishBootOutcome records how the readiness wait ended in the boot-stage
// file, which is the only progress a process that did not start this VM can
// see.
//
// It exists because the boot phase never reached a terminal stage: the
// publisher advanced as far as Incus and stopped, so a reader could watch the
// Incus wait begin and never learn whether it succeeded. That was survivable
// while the only reader was a menubar splash; it is not, now that the CLI
// attaches to a holder over this file and has to decide when to stop waiting
// and print the summary.
//
// A nil publisher (a boot that failed before StepBootStage) is a no-op.
func (h *Host) publishBootOutcome(bootErr error) {
	if h.bootPub == nil {
		return
	}
	if bootErr != nil {
		h.bootPub.advance(bootstage.Failed)
		return
	}
	h.bootPub.advance(bootstage.Ready)
}

// waitForGuestReady runs the Incus readiness wait. Returns nil if the guest
// reached the Incus-ready state, or an error describing why it didn't. Errors
// are non-fatal at the call site (partial reports are still useful) but the
// caller should warn the user rather than pretend everything is fine.
//
// The two ways it can fail are reported differently on purpose. A budget that
// ran out is a boot problem — the guest was too slow, or never came up. A
// CANCELED wait is not a boot problem at all: something released the run while
// the guest was still coming up, and the guest may well have been seconds away
// from ready. Both used to surface as "...: context canceled" under a log line
// that said "timed out", which is a lie in one direction and unactionable in
// the other.
func (h *Host) waitForGuestReady(ctx context.Context) error {
	_, err := h.activeRunner().WaitForIncus(ctx)
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		reason := cancelReason(context.Cause(ctx))
		logging.L().Warn("guest readiness wait was canceled before the guest was ready", "reason", reason, "error", err)
		return fmt.Errorf("boot interrupted (%s): %w", reason, err)
	}
	logging.L().Error("wait for incus", "error", err)
	return err
}

// cancelReason renders the cause of a released run context in words worth
// putting in front of a user. It is pure so the mapping is testable without a
// Host, and it is the whole point of recording a cause: it answers "did we ask
// for this?" — a control-plane stop, versus something outside the process
// (a signal, a killed process group, a parent shutting down).
func cancelReason(cause error) string {
	switch {
	case cause == nil:
		return "cause not recorded"
	case errors.Is(cause, ErrStopRequested):
		return ErrStopRequested.Error()
	case errors.Is(cause, context.Canceled):
		return "canceled by the caller: a signal, or the parent shutting down"
	default:
		// A caller that cancels with a cause of its own (the CLI names the
		// signal it received) says it better than this package can.
		return cause.Error()
	}
}

// onStopErr routes a teardown failure to the Observer.
func (h *Host) onStopErr(step string, err error) { h.obs.TeardownWarning(step, err) }

// stop releases Run's block. It is what the control-plane stop and eject
// handlers call, and it is safe before Run has started (a no-op).
func (h *Host) stop() {
	h.mu.Lock()
	cancel := h.cancel
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// setRunner publishes the runner so the control handlers (registered before the
// VM exists) can reach it.
func (h *Host) setRunner(r *vm.Runner) {
	h.mu.Lock()
	h.runner = r
	h.mu.Unlock()
}

// activeRunner returns the runner, or nil if the VM has not been constructed.
func (h *Host) activeRunner() *vm.Runner {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.runner
}

// lockedConfig mutates the config under the config router's lock, so a
// concurrent `br config get` never reads a half-updated value.
func (h *Host) lockedConfig(mutate func()) {
	if h.cfgRouter == nil {
		mutate()
		return
	}
	h.cfgRouter.Lock()
	defer h.cfgRouter.Unlock()
	mutate()
}

// steps is the ordered lifecycle. Teardown is its exact reverse.
func (h *Host) steps() []step {
	return []step{
		{name: StepCartridge, start: noCtx(h.startCartridge), stop: h.stopCartridge},
		{name: StepUnmountVeto, start: noCtx(h.startUnmountWatch), stop: h.stopUnmountWatch},
		{name: StepControl, start: noCtx(h.startControl), stop: h.stopControl},
		{name: StepServe, start: h.startServe},
		{name: StepRegistry, start: noCtx(h.startRegistry), stop: h.stopRegistry},
		{name: StepConfig, start: noCtx(h.startConfig)},
		{name: StepPorts, start: noCtx(h.startPorts), stop: h.stopPorts},
		{name: StepSSHKeys, start: noCtx(h.startSSHKeys)},
		{name: StepOIDC, start: h.startOIDC, stop: h.stopOIDC},
		{name: StepNTP, start: noCtx(h.startNTP), stop: h.stopNTP},
		{name: StepRunner, start: noCtx(h.startRunner), stop: h.stopRunner},
		{name: StepBootStage, start: noCtx(h.startBootStage), stop: h.stopBootStage},
		{name: StepVM, start: h.startVM},
		{name: StepWebProxy, start: noCtx(h.startWebProxy), stop: h.stopWebProxy},
	}
}

// startCartridge attaches the cartridge image, unless the caller already opened
// one and handed it over. A non-cartridge instance skips this entirely.
func (h *Host) startCartridge() error {
	if h.spec.Kind != instance.KindCartridge || h.adopted {
		return nil
	}
	opened, err := cartridge.Open(h.spec.CartridgePath, h.spec.cartridgeOpenOptions())
	if err != nil {
		return err
	}
	h.cartridge = opened
	return nil
}

// stopCartridge releases the cartridge image. It runs last, after the VMM has
// stopped and released root.img, so the detach is never blocked by the VM.
func (h *Host) stopCartridge() error {
	if h.cartridge == nil {
		return nil
	}
	return h.cartridge.Close()
}

// startControl resolves the base config, refuses to start over a live
// instance, and BINDS the control socket — the ownership token for this
// instance. It does not serve yet; that is StepServe, so that claiming the
// socket and answering on it stay separable. The socket lives inside the
// cartridge mount, which is why this runs after the cartridge is attached.
func (h *Host) startControl() error {
	cfg, err := h.baseConfig()
	if err != nil {
		return err
	}
	h.cfg = cfg

	if control.NewClient(cfg.VMDir).IsRunning() {
		return ErrAlreadyRunning
	}

	// We build the controller explicitly (rather than via NewServer) so a
	// guest-liveness probe can be attached once the VM is running — see
	// ProbeGuest in startVM.
	h.ctrl = control.NewLocalController(h.stop)
	listener, err := control.NewListener(cfg.VMDir, h.ctrl)
	if err != nil {
		return fmt.Errorf("start control server: %w", err)
	}
	h.listener = listener

	// The config handler captures cfg by reference, so it serves values set
	// long after the VM starts.
	h.cfgRouter = control.NewConfigRouter(cfg)
	listener.Router().Mount("config", h.cfgRouter.Router())

	// Handlers must all be registered before the server starts serving, to
	// avoid a handlers-map race.
	h.registerControlHandlers(listener.Router())
	return nil
}

// startServe begins answering on the bound control socket. Every handler is
// registered by then, so the accept loop can never race the handlers map.
func (h *Host) startServe(ctx context.Context) error {
	go h.listener.Start(ctx)
	return nil
}

// stopControl closes the control socket.
func (h *Host) stopControl() error {
	if h.listener == nil {
		return nil
	}
	_ = h.listener.Close()
	return nil
}

// baseConfig resolves the config the overlays are applied to: the caller's
// pre-resolved one when given, else config.Default rooted at the cartridge
// mountpoint (which owns the instance's state) or the spec's state dir.
func (h *Host) baseConfig() (*config.Config, error) {
	if h.spec.Config != nil {
		return h.spec.Config, nil
	}
	stateDir := h.spec.StateDir
	if mp := h.cartridge.Mountpoint(); mp != "" {
		stateDir = mp
	}
	cfg, err := config.Default(stateDir)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}

// startConfig applies the whole overlay chain and brings logging up.
//
// Precedence, weakest first: defaults -> persisted Settings -> disk manifest ->
// CLI overrides -> explicit port preference -> the cartridge. The cartridge is
// last because it is by definition self-contained: its own root.img, state and
// share always win.
func (h *Host) startConfig() error {
	// Settings live under the default state dir (not a custom --state-dir slot
	// or a cartridge), matching the menubar's settings screen. A missing file
	// yields defaults (no-op); an invalid file is logged once logging is up and
	// ignored in favor of defaults rather than aborting start.
	settings, settingsErr := config.LoadSettings(config.DefaultStateDir())
	if settingsErr != nil {
		settings = config.DefaultSettings()
	}
	settings.ApplyTo(h.cfg)

	if h.spec.Manifest != nil {
		if err := h.spec.Manifest.ApplyTo(h.cfg); err != nil {
			return fmt.Errorf("apply disk: %w", err)
		}
	} else if h.cartridge != nil && h.cartridge.Manifest != nil {
		// A cartridge carries its own disk manifest, and when the Host opened
		// the image itself nothing upstream could read it: the CLI used to do
		// this hop, and a holder-spawned boot has no CLI. Only the non-image
		// defaults are taken — the cartridge supplies its own root.img below.
		h.cartridge.Manifest.ApplyDefaultsTo(h.cfg)
	}

	h.spec.applyOverrides(h.cfg)

	if h.spec.Ports != (config.PortAssignment{}) {
		h.cfg.AssignPorts(h.spec.Ports)
	}

	// Roots every per-VM path inside the mounted image and wires the RW share.
	// No-op for a non-cartridge boot.
	h.cartridge.ApplyTo(h.cfg)

	if err := logging.Init(h.cfg.LogPath); err != nil {
		return err
	}
	if settingsErr != nil {
		logging.L().Warn("ignoring invalid settings; using defaults", "err", settingsErr)
	}
	return nil
}

// startPorts reserves this instance's host loopback ports before anything binds
// one. The default instance still PREFERS 6022 / 18443 / 18444 / 15556 / 15557
// so documented URLs, muscle memory, and hand-written ssh configs keep working;
// only an additional instance — which finds those taken — falls back to
// ephemeral ports. The reservations stay BOUND and are handed to the services
// that serve them, so nothing can steal a port in between.
//
// Reservation failure is not fatal: the services fall back to binding the
// well-known ports themselves, exactly as they did before reservations existed.
func (h *Host) startPorts() error {
	set, err := h.reservePorts()
	if err != nil {
		logging.L().Warn("port reservation failed; using the well-known ports directly", "err", err)
		return nil
	}
	h.lockedConfig(func() { h.cfg.AssignPortsFrom(set) })
	h.parkHostListeners(set)
	// The published ports just changed from "whatever the config asked for" to
	// "what we actually bound"; a reader must not be told the stale set.
	h.republishRegistry()
	return nil
}

// stopPorts releases anything not taken by a service (start failed, or that
// service is disabled) rather than leaking it for the process lifetime.
func (h *Host) stopPorts() error {
	if h.cfg == nil {
		return nil
	}
	_ = h.cfg.CloseHostListeners()
	return nil
}

// startSSHKeys materializes the host key pair the guest is provisioned with and
// the OIDC bootstrap identity is derived from.
func (h *Host) startSSHKeys() error {
	keyPair, err := ssh.EnsureKeyPair()
	if err != nil {
		return fmt.Errorf("ssh keys: %w", err)
	}
	h.hostPublicKey = keyPair.PublicKey
	h.lockedConfig(func() { h.cfg.SetSSHKeys(keyPair.PublicKey, keyPair.PrivateKeyPath) })
	return nil
}

// startOIDC starts the local OIDC provider before the VM so the vsock-reverse
// forwarder can dial it as soon as the guest comes up. Failure to start OIDC is
// logged but does not abort start; the mTLS fallback path remains available.
func (h *Host) startOIDC(ctx context.Context) error {
	provider, err := h.startOIDCProvider(ctx)
	switch {
	case errors.Is(err, errOIDCDisabled):
		logging.L().Info("oidc provider disabled by config")
	case err != nil:
		logging.L().Warn("oidc provider not started", "err", err)
	default:
		h.oidc = provider
	}
	return nil
}

// stopOIDC shuts the local OIDC provider down.
func (h *Host) stopOIDC() error {
	if h.oidc == nil {
		return nil
	}
	_ = h.oidc.Stop()
	return nil
}

// startNTP starts the host pseudo-NTP (SNTP) responder before the VM so the
// vsock reverse forwarder can dial it the moment the guest chrony polls. The
// responder serves the HOST clock as a stratum-1 source; the guest coheres to
// the host (not UTC) and works offline over vsock. Non-fatal: chrony retries.
func (h *Host) startNTP() error {
	if h.cfg.LocalNTPPort == 0 || h.cfg.VsockNTPPort == 0 {
		return nil
	}
	responder, err := h.newNTPResponder()
	if err != nil {
		logging.L().Warn("ntp responder not started", "err", err)
		return nil
	}
	responder.Start()
	h.ntp = responder
	return nil
}

// stopNTP shuts the SNTP responder down.
func (h *Host) stopNTP() error {
	if h.ntp == nil {
		return nil
	}
	_ = h.ntp.Stop()
	return nil
}

// startRunner constructs the VM runner and lets the front end render its
// pre-boot banner. The VM itself is not started here — see startVM.
func (h *Host) startRunner() error {
	runner, err := vm.NewRunner(h.cfg)
	if err != nil {
		return fmt.Errorf("create runner: %w", err)
	}
	h.setRunner(runner)

	// --restore: bring the guest up from a saved-state file (and resume it)
	// instead of cold-booting. Used by `br restore` and `br upgrade`.
	if h.spec.RestoreFrom != "" {
		runner.SetRestoreFrom(h.spec.RestoreFrom)
	}

	h.obs.Resolved(h.cfg)
	return nil
}

// stopRunner tears the VMM down, draining the guest first. The budget is the
// one the caller asked for — drainTimeout resolves the Spec's DrainTimeout,
// falling back to DefaultDrainTimeout — so a guest given a longer drain on the
// command line actually gets it here, not only on the unmount-eject path.
func (h *Host) stopRunner() error {
	return h.stopGuest(context.Background(), h.drainTimeout())
}

// startBootStage publishes coarse, human-friendly boot phase to the bootstage
// file for the menubar's starting splash. Driven by the runner's own stage
// events — which fire in every mode (GUI, headless, detached menubar boot) and
// don't depend on racing/parsing the serial console — so a separate process
// that only sees the control socket can still show "Booting Linux… / Setting
// up… / Starting Incus…" as the guest comes up.
func (h *Host) startBootStage() error {
	h.bootPub = newBootStagePublisher(h.cfg.VMDir)
	return nil
}

// stopBootStage clears the published boot phase on the way out.
func (h *Host) stopBootStage() error {
	bootstage.Clear(h.cfg.VMDir)
	return nil
}

// startVM starts the VM and wires everything that needs a running guest.
func (h *Host) startVM(ctx context.Context) error {
	// Attach progress sinks: always the bootstage file publisher, plus the
	// front end's own (the CLI's TTY boot board) when it has one.
	reporters := []vm.Progress{&bootStageProgress{pub: h.bootPub}}
	if p := h.obs.Progress(ctx, h.cfg); p != nil {
		reporters = append(reporters, p)
	}
	runner := h.activeRunner()
	runner.SetProgress(teeProgress(reporters))

	result, err := runner.StartVM(ctx)
	if err != nil {
		h.obs.Failed(err)
		return fmt.Errorf("start vm: %w", err)
	}
	h.endpoint = result.Endpoint

	// Now that the VM (and its vsock device) exists, teach `br status` to probe
	// guest liveness instead of trusting the host run-state alone. A panicked
	// or unreachable guest now reports "unreachable" rather than "running".
	h.ctrl.SetProbe(func(ctx context.Context) error {
		pctx, cancelProbe := context.WithTimeout(ctx, guestProbeTimeout)
		defer cancelProbe()
		return runner.ProbeGuest(pctx)
	})

	// Publish the resolved nested-virt state so `br status` can report whether
	// Incus VMs are available in this guest.
	h.lockedConfig(func() { h.cfg.NestedVirt = runner.NestedVirtState() })

	// Write SSH config after VM starts. The default instance rewrites the shared
	// aggregator (its legacy "Host bladerunner" block); every other instance
	// writes its own config.d/<name> fragment, so two instances no longer
	// clobber each other's O_TRUNC write. The fragment is named with the same
	// instanceName the registry entry carries — for a cartridge that is "demo",
	// not the "bladerunner-demo" its /Volumes mountpoint is called.
	sshConfigPath, err := ssh.WriteConfigFor(h.instanceName(), h.cfg.LocalSSHPort, h.cfg.SSHUser, h.cfg.SSHPrivateKeyPath)
	if err != nil {
		logging.L().Warn("ssh config", "error", err)
		h.republishRegistry()
		return nil
	}
	h.lockedConfig(func() { h.cfg.SSHConfigPath = sshConfigPath })
	// Everything a reader needs — ports, mountpoint, nested-virt — is final now.
	h.republishRegistry()
	return nil
}

// startWebProxy starts the host-side web-UI proxy. It terminates the browser's
// TLS WITHOUT requesting a client certificate (so the browser never shows the
// cert picker), forwarding to Incus over loopback with no client cert of its
// own — so Incus authenticates the browser via OIDC. `br web` points the
// browser here (LocalWebPort) instead of straight at Incus. Non-fatal: a
// failure just means `br web` falls back to the direct Incus URL (with the cert
// prompt).
//
// The proxy binds its own listener, so the reservation held for it is released
// here — as late as possible, which keeps a concurrently starting instance off
// this port until the moment the proxy takes it. It is the one remaining
// reserve-then-rebind window; closing it needs a Listener option on
// webproxy.Options (follow-up).
func (h *Host) startWebProxy() error {
	h.releaseHostListener(config.PortNameWeb)

	proxy, err := webproxy.New(webproxy.Options{
		ListenAddr:   config.LoopbackAddr(h.cfg.LocalWebPort),
		UpstreamAddr: config.LoopbackAddr(h.cfg.LocalAPIPort),
		CertPath:     filepath.Join(h.cfg.VMDir, "webproxy.crt"),
		KeyPath:      filepath.Join(h.cfg.VMDir, "webproxy.key"),
	})
	if err != nil {
		logging.L().Warn("web proxy not created", "err", err)
		return nil
	}
	if err := proxy.Start(); err != nil {
		logging.L().Warn("web proxy not started", "err", err)
		return nil
	}
	h.webProxy = proxy
	return nil
}

// stopWebProxy shuts the web-UI proxy down.
func (h *Host) stopWebProxy() error {
	if h.webProxy == nil {
		return nil
	}
	_ = h.webProxy.Close()
	return nil
}

// registerControlHandlers registers the control commands that back `runner
// upgrade` and `br eject`: reporting the server's build version, pausing+saving
// the guest state, and the clean ACPI shutdown. They are registered before the
// VM exists and reach it through activeRunner once it does.
func (h *Host) registerControlHandlers(router *control.Router) {
	router.HandleFunc(control.CmdServerVersion, func(_ context.Context, _ *control.Request) *control.Message {
		return &control.Message{Response: h.spec.BinaryVersion}
	})
	router.HandleFunc(control.CmdSave, func(_ context.Context, req *control.Request) *control.Message {
		r := h.activeRunner()
		if r == nil {
			return &control.Message{Error: ErrNotStarted.Error()}
		}
		if err := r.SaveState(h.cfg.SavedStatePath); err != nil {
			return &control.Message{Error: err.Error()}
		}
		if req.Args["0"] != control.SaveModePause {
			if err := r.ResumeVM(); err != nil {
				return &control.Message{Error: err.Error()}
			}
		}
		return &control.Message{Response: h.cfg.SavedStatePath}
	})
	router.HandleFunc(control.CmdEject, func(ctx context.Context, req *control.Request) *control.Message {
		// Gracefully (ACPI) shut the guest down and wait for it to stop. Detach
		// of any cartridge is NOT done here: the VMM still holds root.img until
		// the runner is stopped after Run unblocks. We only stop the guest,
		// then release Run so teardown runs its steps in reverse — VMM first,
		// cartridge detach last.
		if err := h.drain(ctx, ejectTimeoutFromArgs(req), ejectForceFromArgs(req)); err != nil {
			return &control.Message{Error: err.Error()}
		}
		return &control.Message{Response: control.RespOK}
	})
}

// ejectTimeoutFromArgs parses the positional timeout (seconds) from an eject
// request, falling back to the default when absent or unparseable.
func ejectTimeoutFromArgs(req *control.Request) time.Duration {
	if v, ok := req.Args["0"]; ok {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return control.DefaultEjectTimeoutSeconds * time.Second
}

// ejectForceFromArgs reports whether the eject request asked for a forced stop.
func ejectForceFromArgs(req *control.Request) bool {
	return req.Args["1"] == control.EjectModeForce
}

// startOIDCProvider boots the local OIDC server, registers the host's own SSH
// public key as the bootstrap admin identity, and returns the running provider.
// Returns errOIDCDisabled (with a nil provider) when OIDC is disabled by config;
// other errors mean startup failed and the caller should log and continue.
func (h *Host) startOIDCProvider(ctx context.Context) (*oidc.Provider, error) {
	cfg := h.cfg
	if cfg.LocalOIDCPort == 0 {
		return nil, errOIDCDisabled
	}

	signingKey, err := oidc.LoadOrCreateSigningKey(cfg.OIDCStateDir)
	if err != nil {
		return nil, fmt.Errorf("signing key: %w", err)
	}

	store := oidc.NewStore(cfg.IdentityDir)
	if err := store.Load(); err != nil {
		return nil, fmt.Errorf("load identities: %w", err)
	}

	// Bootstrap: auto-import the host's SSH public key on first start.
	if h.hostPublicKey != "" && store.Count() == 0 {
		if _, err := store.Add(h.hostPublicKey); err != nil {
			logging.L().Warn("auto-import host key failed", "err", err)
		}
	}

	// Serve on the listener reserved for this instance when there is one; the
	// provider binds ListenAddr itself otherwise. IssuerURL is read from the
	// config, which AssignPorts re-derived from the port actually reserved —
	// formatting it from the port constant here is what used to break OIDC
	// silently at login time.
	ln := cfg.TakeHostListener(config.PortNameOIDC)
	provider, err := oidc.NewProvider(oidc.Config{
		ListenAddr: config.LoopbackAddr(cfg.LocalOIDCPort),
		Listener:   ln,
		IssuerURL:  cfg.OIDCIssuerURL,
		Audience:   cfg.OIDCAudience,
		SigningKey: signingKey,
		Store:      store,
	})
	if err != nil {
		closeListener(config.PortNameOIDC, ln)
		return nil, err
	}
	if err := provider.Start(ctx); err != nil {
		closeListener(config.PortNameOIDC, ln)
		return nil, err
	}
	return provider, nil
}

// reservePorts binds this instance's host loopback ports as a set, preferring
// whatever the config currently asks for (the well-known constants for the
// default instance, a Settings/manifest override otherwise) and falling back to
// ephemeral ports when a preferred one is taken. Reservation is all-or-nothing:
// a partial failure rolls back every port it had bound.
//
// A zero OIDC or NTP port means that service is disabled, so no port is
// reserved for it and AssignPortsFrom leaves the zero in place.
func (h *Host) reservePorts() (*portalloc.Set, error) {
	cfg := h.cfg
	specs := []portalloc.Spec{
		{Name: config.PortNameSSH, Preferred: cfg.LocalSSHPort},
		{Name: config.PortNameAPI, Preferred: cfg.LocalAPIPort},
		{Name: config.PortNameWeb, Preferred: cfg.LocalWebPort},
	}
	if cfg.LocalOIDCPort != 0 {
		specs = append(specs, portalloc.Spec{Name: config.PortNameOIDC, Preferred: cfg.LocalOIDCPort})
	}
	if cfg.LocalNTPPort != 0 {
		specs = append(specs, portalloc.Spec{Name: config.PortNameNTP, Preferred: cfg.LocalNTPPort})
	}
	set, err := portalloc.ReserveSet(specs...)
	if err != nil {
		return nil, fmt.Errorf("reserve instance ports: %w", err)
	}
	logging.L().Info("reserved instance ports",
		"instance", cfg.InstanceName(),
		"ssh", set.Port(config.PortNameSSH),
		"api", set.Port(config.PortNameAPI),
		"web", set.Port(config.PortNameWeb),
		"oidc", set.Port(config.PortNameOIDC),
		"ntp", set.Port(config.PortNameNTP),
	)
	return set, nil
}

// parkHostListeners moves every bound listener out of the reservation set and
// onto the config, where each service takes the one it serves. Handing over the
// live listener — instead of a port number to re-bind — is what removes the
// window in which a second instance could steal the port.
func (h *Host) parkHostListeners(set *portalloc.Set) {
	for _, name := range set.Names() {
		ln, err := set.Detach(name)
		if err != nil {
			logging.L().Warn("could not hand over reserved listener", "port_name", name, "err", err)
			continue
		}
		h.cfg.SetHostListener(name, ln)
	}
}

// releaseHostListener closes a reserved listener whose service binds the
// address itself, immediately before that service binds.
func (h *Host) releaseHostListener(name string) {
	closeListener(name, h.cfg.TakeHostListener(name))
}

// closeListener closes a reserved listener that will not be served, logging
// rather than failing the start.
func closeListener(name string, ln net.Listener) {
	if ln == nil {
		return
	}
	if err := ln.Close(); err != nil {
		logging.L().Warn("could not release reserved listener", "port_name", name, "err", err)
	}
}

// newNTPResponder builds the host SNTP responder on the listener reserved for
// it, or binds the configured address when no reservation was made.
func (h *Host) newNTPResponder() (*timesource.Responder, error) {
	if ln := h.cfg.TakeHostListener(config.PortNameNTP); ln != nil {
		return timesource.NewResponderWithListener(ln), nil
	}
	responder, err := timesource.NewResponder(config.LoopbackAddr(h.cfg.LocalNTPPort))
	if err != nil {
		return nil, fmt.Errorf("bind sntp responder: %w", err)
	}
	return responder, nil
}

// bootStagePublisher writes the coarse boot phase to the bootstage file,
// advancing monotonically (rank-gated; Failed is terminal). Safe for the
// concurrent Begin/Done calls the runner makes from its wait goroutines.
type bootStagePublisher struct {
	mu       sync.Mutex
	stateDir string
	cur      bootstage.Stage
}

// newBootStagePublisher creates the publisher and writes the initial Boot phase
// immediately, so the menubar shows "Booting Linux…" the moment a start begins.
func newBootStagePublisher(stateDir string) *bootStagePublisher {
	p := &bootStagePublisher{stateDir: stateDir}
	p.advance(bootstage.Boot)
	return p
}

func (p *bootStagePublisher) advance(to bootstage.Stage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if to != bootstage.Failed && bootstage.Rank(to) <= bootstage.Rank(p.cur) {
		return
	}
	p.cur = to
	_ = bootstage.Write(p.stateDir, to, time.Now())
}

// bootStageProgress is a vm.Progress sink that maps the runner's stage events
// onto bootstage phases. VMBoot done -> Setup covers the guest's own boot
// (kernel/cloud-init/ssh) between the VM reaching "running" and the Incus wait.
type bootStageProgress struct{ pub *bootStagePublisher }

func (p *bootStageProgress) Begin(stage, _ string, _ time.Duration) {
	switch stage {
	case vm.StageVMBoot:
		p.pub.advance(bootstage.Boot)
	case vm.StageIncusWait:
		p.pub.advance(bootstage.Incus)
	}
}
func (p *bootStageProgress) Substatus(string, string) {}
func (p *bootStageProgress) Done(stage string) {
	if stage == vm.StageVMBoot {
		p.pub.advance(bootstage.Setup)
	}
}
func (p *bootStageProgress) Fail(string, error) { p.pub.advance(bootstage.Failed) }

// teeProgress fans every progress event out to several sinks (the bootstage
// file publisher plus the front end's optional TTY board).
type teeProgress []vm.Progress

func (t teeProgress) Begin(s, l string, b time.Duration) {
	for _, p := range t {
		p.Begin(s, l, b)
	}
}
func (t teeProgress) Substatus(s, m string) {
	for _, p := range t {
		p.Substatus(s, m)
	}
}
func (t teeProgress) Done(s string) {
	for _, p := range t {
		p.Done(s)
	}
}
func (t teeProgress) Fail(s string, e error) {
	for _, p := range t {
		p.Fail(s, e)
	}
}
