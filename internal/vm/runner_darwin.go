//go:build darwin

package vm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Code-Hex/vz/v3"
	"github.com/stuffbucket/bladerunner/internal/config"
	incusctl "github.com/stuffbucket/bladerunner/internal/incus"
	"github.com/stuffbucket/bladerunner/internal/logging"
	"github.com/stuffbucket/bladerunner/internal/provision"
	"github.com/stuffbucket/bladerunner/internal/report"
	"github.com/stuffbucket/bladerunner/internal/ssh"
)

// Guest drain tuning. Every shutdown path (Stop, Eject) goes through the same
// drain: ACPI power request, then wait for a genuine stopped-state transition,
// escalating to a forced stop only when the budget expires.
const (
	// drainRequestStopAttempts is how many ACPI power-button requests a drain
	// issues before relying on the wait/timeout to escalate.
	drainRequestStopAttempts = 3
	// drainForceStopGrace bounds the wait for the VM to reach stopped after an
	// escalation to a forced stop.
	drainForceStopGrace = 10 * time.Second
	// drainStatePollInterval is how often the drain re-reads the VM state while
	// waiting, so a stopped transition is never missed just because another
	// consumer of the shared state-change channel received it first.
	drainStatePollInterval = 250 * time.Millisecond
	// DefaultDrainTimeout is the budget Stop gives the guest to power itself off
	// before escalating to a forced stop. A real guest has to stop Incus, unmount
	// and flush filesystems, so the previous ~6s ceiling routinely cut power
	// mid-write; 60s matches the eject/control-plane default and is long enough
	// for an ordinary systemd shutdown while still bounding a wedged guest.
	DefaultDrainTimeout = 60 * time.Second
)

// StopOutcome records how a shutdown path left the guest, so callers (and the
// log) can tell a clean power-off from a power cut.
type StopOutcome string

const (
	// StopOutcomeNotStarted means there was no VM to stop.
	StopOutcomeNotStarted StopOutcome = "not-started"
	// StopOutcomeAlreadyStopped means the VM had already reached the stopped
	// state, so no ACPI request and no forced stop were issued.
	StopOutcomeAlreadyStopped StopOutcome = "already-stopped"
	// StopOutcomeClean means the guest powered itself off in response to the ACPI
	// request, within the drain budget. The disk image is consistent.
	StopOutcomeClean StopOutcome = "clean"
	// StopOutcomeForced means the VMM was force-stopped (a power cut): either the
	// caller asked for it, or the guest did not power off within the budget. The
	// guest filesystem may need a check on next boot.
	StopOutcomeForced StopOutcome = "forced"
)

// Runner owns one guest VM for its whole lifetime. It provisions the disk image
// and the cloud-init seed, builds the Virtualization.framework configuration,
// starts the machine together with the host-side vsock forwarders and the
// console log, and drains the guest again on shutdown. A Runner is single-use:
// once it has been stopped it cannot start another VM.
//
// Drive the start path (Start, StartVM, WaitForIncus) from one goroutine. The
// shutdown paths (Stop, StopWithTimeout, Eject) and ProbeGuest may be called
// from another one; Stop is idempotent and only the first call does the work.
type Runner struct {
	cfg *config.Config

	vm            *vz.VirtualMachine
	vmConfig      *vz.VirtualMachineConfiguration
	metadata      *runtimeMetadata
	clientCrt     []byte
	clientKey     []byte
	baseImagePath string
	// restoreFrom, when set before StartVM, makes StartVM restore the guest
	// from a saved-state file (and resume it) instead of cold-booting.
	restoreFrom string
	// savedState records that the guest's RAM state has been saved to disk and
	// the VM is paused; Stop then skips the graceful ACPI request and tears the
	// VM down directly (the guest must not resume after a save).
	savedState bool

	forwarders        []*portForwarder
	reverseForwarders []*reversePortForwarder
	consoleLog        *logging.RotatingFile
	progress          Progress
	nestedVirt        string // resolved nested-virt state: enabled|unsupported|disabled
	stopOnce          sync.Once
	stopErr           error
	// stopOutcome records how the last drain left the guest. Written by the
	// shutdown paths, read via LastStopOutcome once they have returned.
	stopOutcome StopOutcome
}

// NestedVirtualizationSupported reports whether the host can run nested VMs
// (Apple Silicon M3+ on macOS 15+). When true, bladerunner enables it so the
// guest's Incus can launch VMs (`incus launch --vm`), not just containers.
func NestedVirtualizationSupported() bool {
	return vz.IsNestedVirtualizationSupported()
}

// NestedVirtState returns the resolved nested-virtualization state for this VM:
// "enabled", "unsupported" (host can't), or "disabled" (opted out via config).
// Empty until the platform has been configured (StartVM).
func (r *Runner) NestedVirtState() string {
	return r.nestedVirt
}

// SetRestoreFrom configures StartVM to restore the guest from a saved-state
// file (produced by SaveState) and resume it, instead of cold-booting. Must be
// called before StartVM.
func (r *Runner) SetRestoreFrom(path string) { r.restoreFrom = path }

// SupportsSaveRestore reports whether the configured VM supports VZ
// save/restore, returning an error explaining why not when it doesn't.
func (r *Runner) SupportsSaveRestore() error {
	if r.vmConfig == nil {
		return errors.New("vm not configured")
	}
	ok, err := r.vmConfig.ValidateSaveRestoreSupport()
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("this VM configuration does not support save/restore")
	}
	return nil
}

// SaveState pauses the guest and writes its machine state to path. On success
// the VM is left paused: callers either ResumeVM for a live snapshot, or Stop
// for an upgrade handoff. The guest must not resume between save and a
// subsequent Stop, or the on-disk image diverges from the saved RAM.
func (r *Runner) SaveState(path string) error {
	if r.vm == nil {
		return errors.New("vm not started")
	}
	if err := r.SupportsSaveRestore(); err != nil {
		return err
	}
	if !r.vm.CanPause() {
		return errors.New("vm is not in a pausable state")
	}
	if err := r.vm.Pause(); err != nil {
		return fmt.Errorf("pause vm: %w", err)
	}
	// VZ refuses to overwrite an existing save file, so clear any stale one.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		_ = r.vm.Resume()
		return fmt.Errorf("remove stale saved state %s: %w", path, err)
	}
	if err := r.vm.SaveMachineStateToPath(path); err != nil {
		_ = r.vm.Resume() // best effort: don't strand a paused VM on failure
		return fmt.Errorf("save vm state: %w", err)
	}
	r.savedState = true

	// Record the snapshot's hardware config + disk stamp alongside the file so
	// restore can rebuild a matching configuration and detect a changed disk.
	// Written while paused (disk frozen). Non-fatal: the save itself succeeded.
	if err := writeSaveMetadata(path, r.cfg.CPUs, r.cfg.MemoryGiB, r.cfg.DiskSizeGiB, r.cfg.GUI, r.cfg.DiskPath, r.effectiveShareTag()); err != nil {
		logging.L().Warn("could not write saved-state metadata sidecar", "err", err)
	}
	return nil
}

// guiModeLabel renders a boot mode for operator-facing messages.
func guiModeLabel(gui bool) string {
	if gui {
		return "gui"
	}
	return "headless"
}

// prepareRestore loads the saved-state sidecar (when present), applies the
// snapshot's hardware configuration so the VZ config matches, and refuses the
// restore if the disk has changed since the snapshot. A missing sidecar (an
// older save) degrades to the current config with no disk check.
func (r *Runner) prepareRestore() error {
	meta, err := LoadSaveMetadata(r.restoreFrom)
	if err != nil {
		if os.IsNotExist(err) {
			logging.L().Warn("no saved-state metadata sidecar; using current config and skipping disk-stamp check", "save", r.restoreFrom)
			return nil
		}
		return fmt.Errorf("read saved-state metadata: %w", err)
	}
	if meta.CPUs > 0 {
		r.cfg.CPUs = meta.CPUs
	}
	if meta.MemoryGiB > 0 {
		r.cfg.MemoryGiB = meta.MemoryGiB
	}
	if meta.DiskSizeGiB > 0 {
		r.cfg.DiskSizeGiB = meta.DiskSizeGiB
	}
	// Graphics devices are fixed when the VZ configuration is built, so a
	// headless<->gui mismatch between the snapshot and this boot would fail deep
	// inside VZ with an opaque error. Refuse early with an actionable message.
	// A sidecar without the field (nil, an older save) skips the check.
	if meta.GUI != nil && *meta.GUI != r.cfg.GUI {
		return fmt.Errorf("refusing restore: saved state is %s but boot requested %s; re-boot with the matching mode", guiModeLabel(*meta.GUI), guiModeLabel(r.cfg.GUI))
	}
	// The VirtioFS directory-sharing topology is fixed when the VZ configuration
	// is built, exactly like graphics, so a share present-vs-absent or a different
	// tag between the snapshot and this boot would fail deep inside VZ. Refuse
	// early with an actionable message. An empty recorded tag (a sidecar from
	// before this field, or a snapshot with no share) only matches a boot with no
	// share, so an older sidecar restored against a no-share boot still passes.
	if meta.ShareTag != r.effectiveShareTag() {
		return fmt.Errorf("refusing restore: saved state share is %q but boot share is %q; re-boot with the matching share configuration", meta.ShareTag, r.effectiveShareTag())
	}
	if err := meta.VerifyDisk(); err != nil {
		return fmt.Errorf("refusing restore: %w", err)
	}
	return nil
}

// Eject performs the cartridge clean-shutdown lifecycle: it issues ACPI power
// requests (RequestStop) and waits up to timeout for the guest to reach the
// stopped state. If the guest does not power off in time, or force is set, it
// escalates to a forced stop. It returns nil once the VM is stopped (or was
// never running). The caller is then free to detach the cartridge image, which
// the VMM has released. This shares the drain state machine with Stop but does
// not consume the sync.Once guard, so a later deferred Stop() remains safe and
// idempotent (and, finding the VM already stopped, will not force anything).
func (r *Runner) Eject(ctx context.Context, timeout time.Duration, force bool) error {
	if r.vm == nil {
		return errors.New("vm not started")
	}
	outcome, err := drainGuest(ctx, r.vm, timeout, force, logging.L())
	r.stopOutcome = outcome
	logDrainOutcome(logging.L(), "eject", outcome, err)
	return err
}

// LastStopOutcome reports how the most recent shutdown path (Stop or Eject) left
// the guest. It is meaningful once that call has returned; before any shutdown
// it is the empty string.
func (r *Runner) LastStopOutcome() StopOutcome { return r.stopOutcome }

// drainTarget is the subset of *vz.VirtualMachine the drain state machine uses.
// It exists so the wait/escalate decisions are testable without a real VM.
type drainTarget interface {
	State() vz.VirtualMachineState
	CanRequestStop() bool
	RequestStop() (bool, error)
	CanStop() bool
	Stop() error
	StateChangedNotify() <-chan vz.VirtualMachineState
}

// drainGuest is the single orderly-shutdown implementation shared by Stop and
// Eject. Unless force is set it presses the ACPI power button and waits up to
// budget for a genuine transition to the stopped state; only when that budget
// expires does it escalate to the destructive vz.Stop(), and it says so. A VM
// that is already stopped is left alone. The returned outcome always describes
// what was attempted, even when the accompanying error is non-nil.
func drainGuest(ctx context.Context, vm drainTarget, budget time.Duration, force bool, log loggerLike) (StopOutcome, error) {
	if vm.State() == vz.VirtualMachineStateStopped {
		return StopOutcomeAlreadyStopped, nil
	}

	if force {
		forceStopTarget(vm, log)
		return StopOutcomeForced, waitForStoppedState(ctx, vm, budget)
	}

	// Press the ACPI power button; the guest's init powers the machine off.
	for i := 0; i < drainRequestStopAttempts && vm.CanRequestStop(); i++ {
		ok, err := vm.RequestStop()
		log.Info("drain: sent ACPI stop request", "attempt", i+1, "accepted", ok, "err", err)
		if err != nil {
			break
		}
	}

	if err := waitForStoppedState(ctx, vm, budget); err != nil {
		// The guest did not power off in time (ACPI ignored, hung, or panicked).
		// Escalating cuts power, so make it a loud, explicit event.
		log.Warn("drain: guest did not power off within budget; escalating to a forced stop (power cut)", "budget", budget, "err", err)
		forceStopTarget(vm, log)
		return StopOutcomeForced, waitForStoppedState(ctx, vm, drainForceStopGrace)
	}
	return StopOutcomeClean, nil
}

// forceStopTarget cuts power to the VM when it can still be stopped.
func forceStopTarget(vm drainTarget, log loggerLike) {
	if !vm.CanStop() {
		return
	}
	log.Warn("drain: forcing VM stop")
	if err := vm.Stop(); err != nil {
		log.Warn("drain: forced stop failed", "err", err)
	}
}

// logDrainOutcome records, at the right severity, whether the guest powered off
// cleanly or had its power cut, so an operator reading the log can tell which
// happened without reconstructing it from timings.
func logDrainOutcome(log loggerLike, path string, outcome StopOutcome, err error) {
	switch outcome {
	case StopOutcomeForced:
		log.Warn("guest was force-stopped; its filesystem may need a check on next boot", "path", path, "outcome", string(outcome), "err", err)
	case StopOutcomeNotStarted, StopOutcomeAlreadyStopped:
		log.Info("no guest drain needed", "path", path, "outcome", string(outcome))
	default:
		log.Info("guest powered off cleanly", "path", path, "outcome", string(outcome))
	}
}

// waitForStoppedState blocks until the VM reaches the stopped state, the
// timeout elapses, or the VM enters the error state. It returns nil once
// stopped.
//
// It also re-polls State() on a ticker rather than trusting the notification
// channel alone: VZ hands every caller the *same* channel, so a concurrent
// consumer (Runner.Wait) can take the stopped event, and a drain that only
// selected on the channel would then sit out its whole budget behind an
// already-stopped VM — and force-stop it.
func waitForStoppedState(ctx context.Context, vm drainTarget, timeout time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	poll := time.NewTicker(drainStatePollInterval)
	defer poll.Stop()
	for {
		switch vm.State() {
		case vz.VirtualMachineStateStopped:
			return nil
		case vz.VirtualMachineStateError:
			return errors.New("vm entered error state during shutdown")
		default:
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("vm did not stop within %s: %w", timeout, waitCtx.Err())
		case <-poll.C:
		case st := <-vm.StateChangedNotify():
			logging.L().Info("drain: vm state changed", "state", st.String())
			switch st {
			case vz.VirtualMachineStateStopped:
				return nil
			case vz.VirtualMachineStateError:
				return errors.New("vm entered error state during shutdown")
			default:
			}
		}
	}
}

// ResumeVM resumes a paused guest (e.g. after a live snapshot save).
func (r *Runner) ResumeVM() error {
	if r.vm == nil {
		return errors.New("vm not started")
	}
	r.savedState = false
	if !r.vm.CanResume() {
		return nil
	}
	return r.vm.Resume()
}

// SetProgress attaches a Progress reporter. Must be called before Start /
// StartVM. Passing nil clears any previous reporter.
func (r *Runner) SetProgress(p Progress) {
	if p == nil {
		r.progress = noopProgress{}
		return
	}
	r.progress = p
}

// StartVMResult contains the initial state after VM starts running.
type StartVMResult struct {
	Endpoint string
}

// NewRunner validates cfg and returns a Runner bound to it. It touches no disk
// and starts nothing: no image is created and no VM exists until StartVM (or
// Start) is called, so a failure here is purely a configuration failure. The
// Runner reports boot progress through a default timed reporter; install your
// own with SetProgress, and any restore source with SetRestoreFrom, before you
// start it.
func NewRunner(cfg *config.Config) (*Runner, error) {
	if cfg == nil {
		return nil, errors.New("config is nil")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Runner{cfg: cfg, progress: NewTimedProgress()}, nil
}

// StartVM provisions and starts the VM, returning as soon as it's running.
// Call WaitForIncus() separately to wait for cloud-init and Incus API readiness.
func (r *Runner) StartVM(ctx context.Context) (*StartVMResult, error) {
	log := logging.L()

	// On restore, adopt the snapshot's hardware config and verify the disk
	// hasn't changed before touching anything.
	if r.restoreFrom != "" {
		if err := r.prepareRestore(); err != nil {
			return nil, err
		}
	}

	log.Info("starting VM provisioning", "name", r.cfg.Name, "vm_dir", r.cfg.VMDir, "cpus", r.cfg.CPUs, "memory_gib", r.cfg.MemoryGiB)

	if err := ensureVMDir(r.cfg); err != nil {
		return nil, err
	}

	log.Info("ensuring client TLS credentials")
	certPEM, keyPEM, err := incusctl.EnsureClientCertificate(r.cfg.ClientCertPath, r.cfg.ClientKeyPath)
	if err != nil {
		return nil, err
	}
	r.clientCrt = certPEM
	r.clientKey = keyPEM

	// On restore the guest is already configured and frozen in the saved
	// state; regenerating cloud-init would needlessly rewrite the seed ISO. The
	// existing ISO file is still attached so the device topology matches the
	// saved configuration.
	if r.restoreFrom == "" {
		log.Info("building cloud-init payload")
		userData, metaData := provision.BuildCloudInit(r.cfg, string(certPEM))
		if err := provision.WriteSeedFiles(r.cfg, userData, metaData); err != nil {
			return nil, err
		}
		if err := provision.BuildCloudInitISO(ctx, r.cfg); err != nil {
			return nil, err
		}
	}

	log.Info("resolving base image and main disk")
	baseImagePath, err := ensureBaseImage(ctx, r.cfg)
	if err != nil {
		return nil, err
	}
	r.baseImagePath = baseImagePath
	if err := ensureMainDisk(r.cfg, baseImagePath); err != nil {
		return nil, err
	}

	md, err := loadOrCreateMetadata(r.cfg)
	if err != nil {
		return nil, err
	}
	r.metadata = md

	log.Info("constructing virtual machine configuration")
	vmCfg, err := r.newVMConfiguration()
	if err != nil {
		return nil, err
	}
	r.vmConfig = vmCfg

	log.Info("creating virtual machine instance")
	vm, err := vz.NewVirtualMachine(vmCfg)
	if err != nil {
		return nil, annotateVZStartError(fmt.Errorf("create vm: %w", err))
	}
	r.vm = vm

	if r.restoreFrom != "" {
		log.Info("restoring saved VM state", "path", r.restoreFrom)
		if err := vm.RestoreMachineStateFromURL(r.restoreFrom); err != nil {
			return nil, fmt.Errorf("restore vm state: %w", err)
		}
		if err := vm.Resume(); err != nil {
			return nil, fmt.Errorf("resume restored vm: %w", err)
		}
	} else {
		log.Info("starting virtual machine")
		if err := vm.Start(); err != nil {
			return nil, annotateVZStartError(fmt.Errorf("start vm: %w", err))
		}
	}

	r.progress.Begin(StageVMBoot, "Waiting for VM to reach running state", 2*time.Minute)
	if err := r.waitForRunning(ctx, 2*time.Minute, func(st vz.VirtualMachineState) {
		msg := fmt.Sprintf("state=%s", st.String())
		r.progress.Substatus(StageVMBoot, msg)
		log.Info("vm state changed", "state", st.String())
	}); err != nil {
		r.progress.Fail(StageVMBoot, err)
		return nil, err
	}
	r.progress.Done(StageVMBoot)

	log.Info("starting localhost forwarders")
	if err := r.startForwarders(); err != nil {
		return nil, err
	}

	endpoint := config.APIEndpoint(r.cfg.LocalAPIPort)
	return &StartVMResult{Endpoint: endpoint}, nil
}

// WaitForIncus waits for the Incus API to become ready and returns a startup report.
//
// r.cfg.WaitForIncus is the ONE budget this wait runs under; it is resolved
// once, in vmhost, from `--timeout` / the persisted Settings / the package
// default (see vmhost.resolveWaitBudget). Nothing here shortens it.
func (r *Runner) WaitForIncus(ctx context.Context) (*report.StartupReport, error) {
	log := logging.L()
	endpoint := config.APIEndpoint(r.cfg.LocalAPIPort)

	incusCtx, cancel := context.WithTimeout(ctx, r.cfg.WaitForIncus)
	defer cancel()

	r.progress.Begin(StageIncusWait, "Waiting for Incus API readiness", r.cfg.WaitForIncus)
	serverInfo, err := incusctl.WaitForServer(incusCtx, endpoint, r.clientCrt, r.clientKey, 4*time.Second, func(p incusctl.WaitProgress) {
		r.progress.Substatus(StageIncusWait, fmt.Sprintf("attempt=%d %s", p.Attempt, summarizeErr(p.LastError)))
	})
	if err != nil {
		r.progress.Fail(StageIncusWait, err)
		// The readiness probe now gates on the Incus API reporting our client as
		// authorized (Auth=="trusted"), not merely "GetServer responded". If we
		// never reach that state the VM is half-started (or its trust store never
		// took our cert), so fail loudly instead of writing a partial report that
		// reads as success. Persist the partial report for diagnostics first.
		//
		// A CANCELED wait is a different event and says so: the budget is not
		// what ended it, so reporting it as a timeout sends the next reader
		// after the wrong thing (they raise --timeout; nothing changes).
		if errors.Is(err, context.Canceled) {
			log.Warn("incus readiness wait canceled before the api was authorized; this is a shutdown, not a boot timeout",
				"endpoint", endpoint, "budget", r.cfg.WaitForIncus.String(), "err", err)
		} else {
			log.Error("incus api never became authorized within its budget",
				"endpoint", endpoint, "budget", r.cfg.WaitForIncus.String(), "err", err)
		}
		reportData := r.makeReport(r.baseImagePath, endpoint, nil)
		if saveErr := report.SaveJSON(r.cfg.ReportPath, reportData); saveErr != nil {
			log.Warn("failed to save partial startup report", "path", r.cfg.ReportPath, "err", saveErr)
		}
		return nil, fmt.Errorf("wait for incus authorization: %w", err)
	}
	r.progress.Done(StageIncusWait)

	log.Info("assembling startup report")
	reportData := r.makeReport(r.baseImagePath, endpoint, serverInfo)
	if err := report.SaveJSON(r.cfg.ReportPath, reportData); err != nil {
		return nil, err
	}
	log.Info("startup report saved", "path", r.cfg.ReportPath)

	return reportData, nil
}

// Start provisions, starts, and waits for Incus. Convenience wrapper for StartVM + WaitForIncus.
func (r *Runner) Start(ctx context.Context) (*report.StartupReport, error) {
	if _, err := r.StartVM(ctx); err != nil {
		return nil, err
	}
	return r.WaitForIncus(ctx)
}

// StartGUI opens the VZ graphical console window onto the running VM. It hands
// the calling thread to the macOS event loop and does not return while the
// window is open, so it must be called on the main thread (main.go holds it
// with runtime.LockOSThread) and any wait that has to keep running -- the Incus
// readiness wait, for one -- belongs on another goroutine. See vmhost.block.
// It returns an error if no VM has been started.
func (r *Runner) StartGUI() error {
	if r.vm == nil {
		return errors.New("vm is not running")
	}
	logging.L().Info("starting GUI console")

	return r.vm.StartGraphicApplication(1920, 1200, vz.WithWindowTitle("Bladerunner Incus VM"), vz.WithController(true))
}

// Wait blocks until the guest leaves the running state, and reports why it
// did: nil once the VM reaches stopped (the ordinary end of a headless run,
// including a shutdown from inside the guest), an error if the VM enters the
// error state, and ctx.Err() if the caller cancels first. A canceled Wait
// leaves the VM alone -- it observes, it does not stop anything -- so a caller
// that wants the guest down must still call Stop or Eject. Waiting on a Runner
// that was never started returns nil at once.
func (r *Runner) Wait(ctx context.Context) error {
	if r.vm == nil {
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			logging.L().Warn("wait context canceled", "err", ctx.Err())
			return ctx.Err()
		case st := <-r.vm.StateChangedNotify():
			logging.L().Info("vm lifecycle event", "state", st.String())
			switch st {
			case vz.VirtualMachineStateError:
				return fmt.Errorf("vm entered error state")
			case vz.VirtualMachineStateStopped:
				return nil
			default:
				// Other states: continue waiting
			}
		}
	}
}

// Stop tears the VM down using the default drain budget (DefaultDrainTimeout).
// It is idempotent: only the first call performs the shutdown, later calls
// return the same error.
func (r *Runner) Stop() error {
	return r.StopWithTimeout(context.Background(), DefaultDrainTimeout)
}

// StopWithTimeout tears the VM down, giving the guest up to budget to power
// itself off via ACPI before escalating to a forced stop. A budget <= 0 means
// DefaultDrainTimeout. Ordering matters for integrity: the guest is drained and
// the disk image flushed *before* the vsock forwarders and the console sink are
// closed, so the guest keeps its host-side channels for the whole shutdown.
// Like Stop, only the first call does the work.
func (r *Runner) StopWithTimeout(ctx context.Context, budget time.Duration) error {
	if budget <= 0 {
		budget = DefaultDrainTimeout
	}
	r.stopOnce.Do(func() {
		log := logging.L()
		log.Info("stopping vm", "drain_budget", budget)

		// 1. Bring the guest down first, while its vsock channels are still up.
		outcome, err := r.drain(ctx, budget, log)
		r.stopOutcome = outcome
		logDrainOutcome(log, "stop", outcome, err)
		r.recordStopErr(err)

		// 2. The guest is down, so the image is quiescent: flush it to stable
		// storage before anything (e.g. a cartridge detach) can pull it away.
		if r.cfg != nil {
			if err := SyncDiskImage(r.cfg.DiskPath); err != nil {
				log.Warn("could not flush disk image to stable storage", "path", r.cfg.DiskPath, "err", err)
				r.recordStopErr(err)
			}
		}

		// 3. Only now tear down the host-side plumbing.
		r.closeForwarders()
		if r.consoleLog != nil {
			r.recordStopErr(r.consoleLog.Close())
		}
	})

	return r.stopErr
}

// drain brings the guest down for Stop. A saved-state guest is paused with its
// RAM already on disk and must not resume, so it is torn down directly instead
// of being asked to power off.
func (r *Runner) drain(ctx context.Context, budget time.Duration, log loggerLike) (StopOutcome, error) {
	if r.vm == nil {
		return StopOutcomeNotStarted, nil
	}
	if r.savedState {
		log.Info("guest state already saved and paused; tearing the VMM down without an ACPI request")
		return drainGuest(ctx, r.vm, budget, true, log)
	}
	return drainGuest(ctx, r.vm, budget, false, log)
}

// recordStopErr keeps the first error seen during shutdown, matching the
// long-standing behavior that Stop reports the earliest failure.
func (r *Runner) recordStopErr(err error) {
	if err != nil && r.stopErr == nil {
		r.stopErr = err
	}
}

// SyncDiskImage flushes the VM's disk image at path to stable storage. On
// darwin os.File.Sync issues F_FULLFSYNC, so this survives a host power loss,
// not merely a process crash. Call it once the guest has stopped (the image is
// then quiescent) and before the image can be detached or copied. An empty path
// or a missing file is not an error: there is nothing to flush.
func SyncDiskImage(path string) error {
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open disk image %s for sync: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync disk image %s: %w", path, err)
	}
	return nil
}

func (r *Runner) closeForwarders() {
	for _, f := range r.forwarders {
		r.recordStopErr(f.Close())
	}
	for _, f := range r.reverseForwarders {
		r.recordStopErr(f.Close())
	}
}

// loggerLike is the subset of charmlog.Logger used by stop helpers.
type loggerLike interface {
	Info(msg any, keyvals ...any)
	Warn(msg any, keyvals ...any)
}

func (r *Runner) waitForRunning(ctx context.Context, timeout time.Duration, onState func(vz.VirtualMachineState)) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if onState != nil {
		onState(r.vm.State())
	}

	for {
		if r.vm.State() == vz.VirtualMachineStateRunning {
			return nil
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("vm did not become running: %w", waitCtx.Err())
		case st := <-r.vm.StateChangedNotify():
			if onState != nil {
				onState(st)
			}
			switch st {
			case vz.VirtualMachineStateRunning:
				return nil
			case vz.VirtualMachineStateError:
				return errors.New("vm entered error state during startup")
			case vz.VirtualMachineStateStopped:
				return errors.New("vm stopped during startup")
			default:
				// Other states (Starting, Pausing, etc.): continue waiting
			}
		}
	}
}

// ProbeGuest checks guest liveness by opening a vsock connection to the
// in-guest SSH bridge port and immediately closing it. A successful connect
// means the guest kernel is alive and the vsock SSH forwarder is listening;
// an error (typically ECONNRESET) means the guest is unreachable — kernel
// panic, not yet booted, or the bridge is down. It returns an error when the
// VM or its socket device is not yet available. The ctx bounds how long the
// (blocking, cgo) dial may take.
func (r *Runner) ProbeGuest(ctx context.Context) error {
	if r.vm == nil {
		return errors.New("vm not started")
	}
	socketDevices := r.vm.SocketDevices()
	if len(socketDevices) == 0 {
		return errors.New("vm has no virtio socket device")
	}
	device := socketDevices[0]

	type dialResult struct {
		conn net.Conn
		err  error
	}
	ch := make(chan dialResult, 1)
	go func() {
		conn, err := device.Connect(r.cfg.VsockSSHPort)
		ch <- dialResult{conn: conn, err: err}
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case res := <-ch:
		if res.err != nil {
			return res.err
		}
		_ = res.conn.Close()
		return nil
	}
}

func (r *Runner) startForwarders() error {
	socketDevices := r.vm.SocketDevices()
	if len(socketDevices) == 0 {
		return errors.New("vm has no virtio socket device configured")
	}
	device := socketDevices[0]

	dial := func(port uint32) (net.Conn, error) {
		return device.Connect(port)
	}

	sshForward := r.hostForwarder("ssh", config.PortNameSSH, r.cfg.LocalSSHPort, r.cfg.VsockSSHPort, dial)
	if err := sshForward.Start(); err != nil {
		return fmt.Errorf("start ssh forwarder: %w", err)
	}

	apiForward := r.hostForwarder("incus-api", config.PortNameAPI, r.cfg.LocalAPIPort, r.cfg.VsockAPIPort, dial)
	if err := apiForward.Start(); err != nil {
		_ = sshForward.Close()
		return fmt.Errorf("start api forwarder: %w", err)
	}

	r.forwarders = []*portForwarder{sshForward, apiForward}
	logging.L().Info("forwarders active", "ssh", config.LoopbackAddr(r.cfg.LocalSSHPort), "api", config.LoopbackAddr(r.cfg.LocalAPIPort))

	r.startOIDCReverseForwarder(device)
	r.startNTPReverseForwarder(device)

	return nil
}

// hostForwarder builds the forwarder for one host loopback service, preferring
// a listener the caller already bound for that port (parked on the config by
// whoever reserved the instance's port set) over binding the address here.
// Taking the pre-bound listener is what removes the window in which another
// instance could steal the port between reservation and bind; falling back to
// listenAddr keeps every existing caller — which reserves nothing — working
// exactly as before.
func (r *Runner) hostForwarder(name, portName string, hostPort int, guestPort uint32, dial func(uint32) (net.Conn, error)) *portForwarder {
	if ln := r.cfg.TakeHostListener(portName); ln != nil {
		return newPortForwarderWithListener(name, ln, guestPort, dial)
	}
	return newPortForwarder(name, config.LoopbackAddr(hostPort), guestPort, dial)
}

// startOIDCReverseForwarder wires the host-side OIDC provider so it is reachable
// from inside the guest via vsock. Failure is logged and ignored: the mTLS
// fallback path keeps Incus access working without OIDC.
func (r *Runner) startOIDCReverseForwarder(device *vz.VirtioSocketDevice) {
	if r.cfg.LocalOIDCPort == 0 || r.cfg.VsockOIDCPort == 0 {
		return
	}
	vsockLn, err := device.Listen(r.cfg.VsockOIDCPort)
	if err != nil {
		logging.L().Warn("could not start oidc vsock listener", "err", err)
		return
	}
	oidcReverse := newReversePortForwarder(
		"oidc",
		config.LoopbackAddr(r.cfg.LocalOIDCPort),
		vsockLn,
	)
	if err := oidcReverse.Start(); err != nil {
		_ = vsockLn.Close()
		logging.L().Warn("could not start oidc reverse forwarder", "err", err)
		return
	}
	r.reverseForwarders = append(r.reverseForwarders, oidcReverse)
}

// startNTPReverseForwarder wires the host pseudo-NTP (SNTP) responder so the
// guest chrony can reach it over vsock. Failure is logged and ignored: chrony
// retries each poll and the guest still boots.
func (r *Runner) startNTPReverseForwarder(device *vz.VirtioSocketDevice) {
	if r.cfg.LocalNTPPort == 0 || r.cfg.VsockNTPPort == 0 {
		return
	}
	vsockLn, err := device.Listen(r.cfg.VsockNTPPort)
	if err != nil {
		logging.L().Warn("could not start ntp vsock listener", "err", err)
		return
	}
	ntpReverse := newReversePortForwarder(
		"ntp",
		config.LoopbackAddr(r.cfg.LocalNTPPort),
		vsockLn,
	)
	if err := ntpReverse.Start(); err != nil {
		_ = vsockLn.Close()
		logging.L().Warn("could not start ntp reverse forwarder", "err", err)
		return
	}
	r.reverseForwarders = append(r.reverseForwarders, ntpReverse)
}

func (r *Runner) makeReport(baseImagePath, endpoint string, server *incusctl.ServerInfo) *report.StartupReport {
	sshEndpoint := fmt.Sprintf("127.0.0.1:%d", r.cfg.LocalSSHPort)
	apiEndpoint := fmt.Sprintf("127.0.0.1:%d", r.cfg.LocalAPIPort)

	// Write SSH config file for easy VM access
	var sshCommand string
	var sshConfigPath string
	if r.cfg.SSHPrivateKeyPath != "" {
		// Per-instance: the default instance rewrites the shared aggregator (its
		// legacy "Host bladerunner" block), any other instance writes its own
		// config.d/<name> fragment. Before this split, a second instance's report
		// overwrote the first's ssh config with its own port.
		instance := r.cfg.InstanceName()
		configPath, err := ssh.WriteConfigFor(instance, r.cfg.LocalSSHPort, r.cfg.SSHUser, r.cfg.SSHPrivateKeyPath)
		if err != nil {
			logging.L().Warn("failed to write SSH config", "err", err)
			sshCommand = fmt.Sprintf("ssh -p %d -i %s %s@127.0.0.1", r.cfg.LocalSSHPort, r.cfg.SSHPrivateKeyPath, r.cfg.SSHUser)
		} else {
			sshConfigPath = configPath
			r.cfg.SSHConfigPath = configPath
			sshCommand = ssh.CommandFor(configPath, instance)
		}
	} else {
		sshCommand = fmt.Sprintf("ssh -p %d %s@127.0.0.1", r.cfg.LocalSSHPort, r.cfg.SSHUser)
	}

	data := &report.StartupReport{
		GeneratedAt: time.Now().UTC(),
		Host: report.HostInfo{
			OS:           runtime.GOOS,
			Arch:         runtime.GOARCH,
			CPUCount:     runtime.NumCPU(),
			RequestedCPU: r.cfg.CPUs,
		},
		VM: report.VMInfo{
			Name:          r.cfg.Name,
			Hostname:      r.cfg.Hostname,
			Directory:     r.cfg.VMDir,
			DiskPath:      r.cfg.DiskPath,
			DiskSizeGiB:   r.cfg.DiskSizeGiB,
			MemoryGiB:     r.cfg.MemoryGiB,
			GuestArch:     runtime.GOARCH,
			GUIEnabled:    r.cfg.GUI,
			ConsoleLog:    r.cfg.ConsoleLogPath,
			CloudInitISO:  r.cfg.CloudInitISO,
			BaseImagePath: baseImagePath,
			BaseImageURL:  r.cfg.BaseImageURL,
		},
		Network: report.NetInfo{
			Mode:             r.cfg.NetworkMode,
			BridgeInterface:  bridgeField(r.cfg),
			MACAddress:       r.metadata.MACAddress,
			LocalSSHEndpoint: sshEndpoint,
			LocalAPIEndpoint: apiEndpoint,
			DashboardURL:     fmt.Sprintf("https://%s%s", apiEndpoint, r.cfg.DashboardPath),
		},
		Access: report.Access{
			SSHCommand:          sshCommand,
			SSHConfigPath:       sshConfigPath,
			SSHKeyPath:          r.cfg.SSHPrivateKeyPath,
			RESTExample:         fmt.Sprintf("curl --cert %s --key %s -k %s/1.0", r.cfg.ClientCertPath, r.cfg.ClientKeyPath, endpoint),
			GoClientExamplePath: filepath.Join(r.cfg.VMDir, "incus-client-example.go"),
			ClientCertPath:      r.cfg.ClientCertPath,
			ClientKeyPath:       r.cfg.ClientKeyPath,
			LogPath:             r.cfg.LogPath,
		},
	}

	if server != nil {
		data.Incus = report.IncusInfo{
			ServerVersion: server.ServerVersion,
			APIVersion:    server.APIVersion,
			Auth:          server.Auth,
			ServerName:    server.ServerName,
			Addresses:     append([]string{}, server.Addresses...),
			APIExtensions: server.APIExtensions,
		}
	}

	_ = os.WriteFile(data.Access.GoClientExamplePath, []byte(goClientExample(r.cfg.ClientCertPath, r.cfg.ClientKeyPath, r.cfg.LocalAPIPort)), 0o644)
	return data
}

func bridgeField(cfg *config.Config) string {
	if cfg.NetworkMode == config.NetworkModeBridged {
		return cfg.BridgeInterface
	}
	return ""
}

func summarizeErr(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	const maxLen = 64
	if len(msg) > maxLen {
		return msg[:maxLen-3] + "..."
	}
	return msg
}
