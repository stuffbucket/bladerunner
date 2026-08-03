//go:build !darwin

package vm

import (
	"context"
	"errors"
	"time"

	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/report"
)

// Runner is the non-darwin build stub for the darwin Runner in
// runner_darwin.go. Bladerunner drives VMs through Virtualization.framework,
// which exists on macOS only, so no VM can be run here; the type exists so that
// packages depending on internal/vm still compile and test on Linux (CI runs
// the suite there). NewRunner therefore never returns a Runner.
//
// The rule for each method, rather than a list of exceptions that goes stale:
// a method that DOES something reports the unsupported error, and a method a
// caller runs on a cleanup or reporting path answers truthfully for a Runner
// that was never started, because an error there would be noise a caller cannot
// act on. Parity with darwin is held by TestEveryDarwinRunnerMethodHasANonDarwinStub,
// which derives both method sets from the source; a new darwin method fails that
// test until it is stubbed here.
type Runner struct{}

// StartVMResult mirrors the darwin StartVMResult so callers compile off darwin.
// Nothing on this platform ever produces one.
type StartVMResult struct {
	Endpoint string
}

// NewRunner is the non-darwin stub; on darwin it validates the config and
// returns a Runner ready to start a VM. Here it always fails, so the caller
// stops at the first step instead of holding an unusable Runner.
func NewRunner(*config.Config) (*Runner, error) {
	return nil, errors.New("bladerunner requires macOS (darwin)")
}

// Start is the non-darwin stub; on darwin it runs StartVM then WaitForIncus and
// returns the startup report.
func (r *Runner) Start(context.Context) (*report.StartupReport, error) {
	return nil, errors.New("unsupported platform")
}

// StartVM is the non-darwin stub; on darwin it provisions the disk and
// cloud-init seed, boots the guest and starts the host-side forwarders.
func (r *Runner) StartVM(context.Context) (*StartVMResult, error) {
	return nil, errors.New("unsupported platform")
}

// WaitForIncus is the non-darwin stub; on darwin it waits for the guest Incus
// API to authorize the host client, then writes the startup report.
func (r *Runner) WaitForIncus(context.Context) (*report.StartupReport, error) {
	return nil, errors.New("unsupported platform")
}

// StartGUI is the non-darwin stub; on darwin it opens the VZ console window on
// the main thread.
func (r *Runner) StartGUI() error { return errors.New("unsupported platform") }

// Wait is the non-darwin stub; on darwin it blocks until the guest stops or
// enters the error state.
func (r *Runner) Wait(context.Context) error { return errors.New("unsupported platform") }

// Stop is the non-darwin stub; on darwin it drains the guest and flushes the
// disk image. Nothing was started here, so it succeeds and lets a deferred
// Stop stay harmless.
func (r *Runner) Stop() error { return nil }

// StopWithTimeout is the non-darwin stub; on darwin it drains the guest under
// the given budget before escalating to a forced stop. It matches Stop rather
// than reporting the unsupported error, for Stop's reason: it runs on a cleanup
// path, nothing was started here, and an error would be noise. The budget is
// discarded because there is no guest to give it to.
func (r *Runner) StopWithTimeout(context.Context, time.Duration) error { return nil }

// StopOutcome is the non-darwin counterpart of the darwin StopOutcome in
// runner_darwin.go. Nothing here can stop a guest, so the only value this build
// ever produces is StopOutcomeNotStarted.
type StopOutcome string

// StopOutcomeNotStarted reports that there was no guest to stop.
const StopOutcomeNotStarted StopOutcome = "not-started"

// LastStopOutcome is the non-darwin stub; on darwin it reports how the most
// recent Stop or Eject left the guest. It answers "not-started" rather than the
// unsupported error, for the reason Stop and StopWithTimeout do the same: it is
// read on a reporting path where an error would be noise, and nothing was ever
// started here, so "not-started" is the truthful answer rather than a placeholder.
func (r *Runner) LastStopOutcome() StopOutcome { return StopOutcomeNotStarted }

// SetProgress is the non-darwin stub; on darwin it attaches the boot-progress
// reporter. There is no boot to report on, so it discards p.
func (r *Runner) SetProgress(Progress) {}

// ProbeGuest is the non-darwin stub; on darwin it dials the in-guest vsock SSH
// bridge to test that the guest is alive.
func (r *Runner) ProbeGuest(context.Context) error { return errors.New("unsupported platform") }

// NestedVirtState is the non-darwin stub; on darwin it reports whether nested
// virtualization was enabled for the guest.
func (r *Runner) NestedVirtState() string { return "unsupported" }

// SetRestoreFrom is the non-darwin stub; on darwin it makes the next StartVM
// restore from a saved-state file instead of cold-booting.
func (r *Runner) SetRestoreFrom(string) {}

// SupportsSaveRestore is the non-darwin stub; on darwin it asks VZ whether the
// built VM configuration can be saved and restored.
func (r *Runner) SupportsSaveRestore() error { return errors.New("unsupported platform") }

// SaveState is the non-darwin stub; on darwin it pauses the guest and writes
// its machine state to the given path.
func (r *Runner) SaveState(string) error { return errors.New("unsupported platform") }

// ResumeVM is the non-darwin stub; on darwin it resumes a guest paused by
// SaveState.
func (r *Runner) ResumeVM() error { return errors.New("unsupported platform") }

// Eject is the non-darwin stub; on darwin it powers the guest off cleanly so
// the cartridge image can be detached, forcing the stop only on timeout or on
// request.
func (r *Runner) Eject(context.Context, time.Duration, bool) error {
	return errors.New("unsupported platform")
}

// NestedVirtualizationSupported is always false off darwin.
func NestedVirtualizationSupported() bool { return false }
