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

// Eject tuning.
const (
	// ejectRequestStopAttempts is how many ACPI power-button requests Eject
	// issues before relying on the wait/timeout to escalate.
	ejectRequestStopAttempts = 3
	// ejectForceStopGrace bounds the wait for the VM to reach stopped after a
	// forced stop.
	ejectForceStopGrace = 10 * time.Second
)

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
// the VMM has released. This composes the existing stop primitives rather than
// reusing Stop() (which is sync.Once-guarded and combines graceful+force
// unconditionally); a later deferred Stop() remains safe and idempotent.
func (r *Runner) Eject(ctx context.Context, timeout time.Duration, force bool) error {
	if r.vm == nil {
		return errors.New("vm not started")
	}
	log := logging.L()

	if force {
		r.forceStopVMIfNeeded(log)
		return r.waitForStopped(ctx, timeout)
	}

	// Issue the ACPI power button a few times; the guest's logind powers off.
	for i := 0; i < ejectRequestStopAttempts && r.vm.CanRequestStop(); i++ {
		ok, err := r.vm.RequestStop()
		log.Info("eject: sent ACPI stop request", "attempt", i+1, "accepted", ok, "err", err)
		if err != nil {
			break
		}
	}

	if err := r.waitForStopped(ctx, timeout); err != nil {
		// The guest did not power off in time (e.g. ACPI ignored / hung). Force it
		// down so the cartridge can be detached.
		log.Warn("eject: guest did not power off gracefully; forcing stop", "err", err)
		r.forceStopVMIfNeeded(log)
		return r.waitForStopped(ctx, ejectForceStopGrace)
	}
	return nil
}

// waitForStopped blocks until the VM reaches the stopped state, the timeout
// elapses, or the VM enters the error state. It returns nil once stopped.
func (r *Runner) waitForStopped(ctx context.Context, timeout time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		switch r.vm.State() {
		case vz.VirtualMachineStateStopped:
			return nil
		case vz.VirtualMachineStateError:
			return errors.New("vm entered error state during eject")
		default:
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("vm did not stop within %s: %w", timeout, waitCtx.Err())
		case st := <-r.vm.StateChangedNotify():
			logging.L().Info("eject: vm state changed", "state", st.String())
			switch st {
			case vz.VirtualMachineStateStopped:
				return nil
			case vz.VirtualMachineStateError:
				return errors.New("vm entered error state during eject")
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

	endpoint := fmt.Sprintf("https://127.0.0.1:%d", r.cfg.LocalAPIPort)
	return &StartVMResult{Endpoint: endpoint}, nil
}

// WaitForIncus waits for the Incus API to become ready and returns a startup report.
func (r *Runner) WaitForIncus(ctx context.Context) (*report.StartupReport, error) {
	log := logging.L()
	endpoint := fmt.Sprintf("https://127.0.0.1:%d", r.cfg.LocalAPIPort)

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
		log.Error("incus api never became authorized before timeout", "endpoint", endpoint, "err", err)
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

func (r *Runner) StartGUI() error {
	if r.vm == nil {
		return errors.New("vm is not running")
	}
	logging.L().Info("starting GUI console")

	return r.vm.StartGraphicApplication(1920, 1200, vz.WithWindowTitle("Bladerunner Incus VM"), vz.WithController(true))
}

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

func (r *Runner) Stop() error {
	r.stopOnce.Do(func() {
		log := logging.L()
		log.Info("stopping vm and forwarders")
		r.closeForwarders()

		if r.consoleLog != nil {
			// Defer until after VM is stopped so we don't kill the
			// serial sink while the guest is still writing.
			defer func() {
				if err := r.consoleLog.Close(); err != nil && r.stopErr == nil {
					r.stopErr = err
				}
			}()
		}

		if r.vm == nil {
			return
		}

		r.requestStopVM(log)
		r.forceStopVMIfNeeded(log)
	})

	return r.stopErr
}

func (r *Runner) closeForwarders() {
	for _, f := range r.forwarders {
		if err := f.Close(); err != nil && r.stopErr == nil {
			r.stopErr = err
		}
	}
	for _, f := range r.reverseForwarders {
		if err := f.Close(); err != nil && r.stopErr == nil {
			r.stopErr = err
		}
	}
}

func (r *Runner) requestStopVM(log loggerLike) {
	if r.savedState {
		// State already saved and the guest is paused; a graceful ACPI request
		// would only stall. forceStopVMIfNeeded tears it down directly.
		return
	}
	for i := 0; i < 3 && r.vm.CanRequestStop(); i++ {
		ok, err := r.vm.RequestStop()
		log.Info("sent stop request", "attempt", i+1, "accepted", ok, "err", err)
		if err != nil && r.stopErr == nil {
			r.stopErr = err
		}
		time.Sleep(2 * time.Second)
	}
}

func (r *Runner) forceStopVMIfNeeded(log loggerLike) {
	if !r.vm.CanStop() {
		return
	}
	if err := r.vm.Stop(); err != nil {
		log.Warn("forced stop failed", "err", err)
		if r.stopErr == nil {
			r.stopErr = err
		}
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

	sshForward := newPortForwarder(
		"ssh",
		fmt.Sprintf("127.0.0.1:%d", r.cfg.LocalSSHPort),
		r.cfg.VsockSSHPort,
		dial,
	)
	if err := sshForward.Start(); err != nil {
		return fmt.Errorf("start ssh forwarder: %w", err)
	}

	apiForward := newPortForwarder(
		"incus-api",
		fmt.Sprintf("127.0.0.1:%d", r.cfg.LocalAPIPort),
		r.cfg.VsockAPIPort,
		dial,
	)
	if err := apiForward.Start(); err != nil {
		_ = sshForward.Close()
		return fmt.Errorf("start api forwarder: %w", err)
	}

	r.forwarders = []*portForwarder{sshForward, apiForward}
	logging.L().Info("forwarders active", "ssh", fmt.Sprintf("127.0.0.1:%d", r.cfg.LocalSSHPort), "api", fmt.Sprintf("127.0.0.1:%d", r.cfg.LocalAPIPort))

	r.startOIDCReverseForwarder(device)
	r.startNTPReverseForwarder(device)

	return nil
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
		fmt.Sprintf("127.0.0.1:%d", r.cfg.LocalOIDCPort),
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
		fmt.Sprintf("127.0.0.1:%d", r.cfg.LocalNTPPort),
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
		configPath, err := ssh.WriteSSHConfig(r.cfg.LocalSSHPort, r.cfg.SSHUser, r.cfg.SSHPrivateKeyPath)
		if err != nil {
			logging.L().Warn("failed to write SSH config", "err", err)
			sshCommand = fmt.Sprintf("ssh -p %d -i %s %s@127.0.0.1", r.cfg.LocalSSHPort, r.cfg.SSHPrivateKeyPath, r.cfg.SSHUser)
		} else {
			sshConfigPath = configPath
			r.cfg.SSHConfigPath = configPath
			sshCommand = ssh.Command(configPath)
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
