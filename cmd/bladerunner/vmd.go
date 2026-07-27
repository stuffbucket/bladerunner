package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/cartridge"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/instance"
	"github.com/stuffbucket/bladerunner/internal/logging"
	"github.com/stuffbucket/bladerunner/internal/vmhost"
)

// `br vmd` is the HOLDER: the minimal process that owns one VM and outlives
// whatever spawned it.
//
// It is a hidden subcommand of `br` and is started by re-exec of
// os.Executable(), not as a separate binary. That is deliberate and it is the
// single most consequential decision in this file. The Virtualization.framework
// entitlement is granted per BINARY: a `cmd/br-vmd` would have to be codesigned
// with vz.entitlements in the goreleaser build hook, in `make sign` and in
// `make build-release`; it would have to be embedded in Bladerunner.app as
// nested signed code and included in notarization; and resolving the path to a
// sibling binary breaks under `go run`, under a relocated Homebrew prefix,
// inside the .app and in a dev worktree. Re-exec has none of those failure
// modes, and it is already the pattern startVMDetachedAndWait uses.
//
// "Minimal" is paid for elsewhere: every line of holder logic lives in
// internal/vmhost, which imports no cobra, no systray and nothing Cocoa. This
// file is a shim — flags in, vmhost.Spec out, Run.

// vmdFlags are the holder's flags. They are deliberately few: a holder is
// configured by the state directory (or cartridge) it is pointed at, not by a
// second copy of every `br start` knob.
var vmdFlags struct {
	stateDir      string
	cartridgePath string
	name          string
	gui           bool
	drainTimeout  time.Duration
}

// errVMDStateDirRequired is returned when `br vmd` is invoked without the one
// argument it cannot default: which instance to hold. A holder is spawned by
// the manager, which always knows the answer, so guessing here would only ever
// hide a bug in the spawner.
var errVMDStateDirRequired = errors.New("--state-dir is required: br vmd holds one specific instance and never guesses which")

var vmdCmd = &cobra.Command{
	Use:   "vmd",
	Short: "Hold a VM instance (internal)",
	Long: `Own one VM instance for as long as it runs.

br vmd is an internal command. It is spawned by bladerunner itself as a
detached process that outlives the CLI and the menubar, and it is not intended
to be run by hand.`,
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE:   runVMD,
	// Hidden commands render nowhere — cobra gates both the group buckets and
	// the "Additional Commands" fallback on IsAvailableCommand — but the group
	// is still assigned, both because the holder genuinely is a lifecycle verb
	// and because the repo requires every registered command to carry one.
	GroupID: groupLifecycle,
}

func init() {
	f := vmdCmd.Flags()
	f.StringVar(&vmdFlags.stateDir, "state-dir", "", "State directory of the instance to hold (required)")
	f.StringVar(&vmdFlags.cartridgePath, "cartridge", "", "Cartridge image to attach and boot")
	f.StringVar(&vmdFlags.name, "name", "", "Instance name (default: derived from the state directory)")
	f.BoolVar(&vmdFlags.gui, "gui", false, "Open the GUI console window")
	f.DurationVar(&vmdFlags.drainTimeout, "drain-timeout", vmhost.DefaultDrainTimeout, "Budget for the orderly guest shutdown")

	// root.go's init registers the groups, and Go runs a package's init
	// functions in file-name order, so "root.go" is always ahead of "vmd.go"
	// and the GroupID above is registered by the time AddCommand checks it.
	rootCmd.AddCommand(vmdCmd)
}

// runVMD holds one instance until it is drained, ejected or (in GUI mode) its
// window is closed.
func runVMD(cmd *cobra.Command, _ []string) error {
	spec, err := buildVMDSpec()
	if err != nil {
		return err
	}
	host, err := vmhost.New(spec)
	if err != nil {
		return err
	}

	// WithCancelCause: the holder's run context records WHY it was released, so a
	// wait cut short by the escalation path says "forced down" rather than
	// reporting a bare cancellation that reads like a boot timeout.
	ctx, cancelCause := context.WithCancelCause(cmd.Context())
	defer cancelCause(nil)
	cancel := context.CancelFunc(func() { cancelCause(errForcedShutdown) })

	// SIGTERM ONLY. A holder is started with setsid and has no controlling
	// terminal, so SIGINT is not a signal it can meaningfully receive; SIGTERM
	// is what `br stop`, launchd and a logout send. It means ORDERLY EJECT, so
	// it is routed into Drain and not into cancel() — canceling would release
	// Run and start teardown while the guest was still running, which is the
	// bug this whole workstream exists to remove.
	signals := make(chan os.Signal, vmdSignalBuffer)
	signal.Notify(signals, syscall.SIGTERM)
	defer signal.Stop(signals)
	go vmdSignalLoop(ctx, signals, host, cancel, spec.DrainTimeout)

	// A holder that lost the race for the instance must say so in the same
	// actionable words the foreground start uses: its log is the only place
	// anyone will read it, and "another bladerunner process holds this
	// instance" alone names neither the process nor the way out.
	return explainHostError(host.Run(ctx))
}

// buildVMDSpec turns the holder flags into the serializable description of the
// instance to run. It is pure: it validates and maps, and touches nothing.
func buildVMDSpec() (vmhost.Spec, error) {
	if vmdFlags.stateDir == "" {
		return vmhost.Spec{}, errVMDStateDirRequired
	}

	spec := vmhost.Spec{
		Kind:          instance.KindFlat,
		StateDir:      vmdFlags.stateDir,
		Name:          vmdFlags.name,
		DrainTimeout:  vmdFlags.drainTimeout,
		BinaryVersion: version,
		Overrides:     vmhost.Overrides{GUI: vmdFlags.gui},
		ChangedFlags:  vmdChangedFlags(),
	}
	if vmdFlags.cartridgePath != "" {
		spec.Kind = instance.KindCartridge
		spec.CartridgePath = vmdFlags.cartridgePath
		// The holder attaches the cartridge itself (nothing handed it an open
		// one), so it needs the conventional per-instance mountpoint. An
		// unnamed holder takes the cartridge's own name rather than mounting
		// every cartridge on top of <stateDir>/mnt.
		spec.Mountpoint = cartridge.MountpointFor(vmdFlags.stateDir, vmdMountName())
	}
	return spec, nil
}

// vmdMountName is the mount-slot name for a cartridge holder: the explicit
// --name when given, otherwise the cartridge image's own basename.
func vmdMountName() string {
	if vmdFlags.name != "" {
		return vmdFlags.name
	}
	return cartridge.NameFromPath(vmdFlags.cartridgePath)
}

// vmdChangedFlags reports which overrides the holder actually asserts. Only
// --gui maps to an override, and only when it was passed: a holder must not
// clobber the instance's persisted Settings with a flag default, exactly as
// `br start` must not.
func vmdChangedFlags() []string {
	if vmdFlags.gui {
		return []string{"gui"}
	}
	return nil
}

// errForcedShutdown is the cause recorded when a second shutdown signal cuts the
// guest's power. It is never returned; it is what context.Cause reports on the
// holder's run context, so anything waiting on that context can say what ended
// it (see vmhost.ErrStopRequested for the orderly counterpart).
var errForcedShutdown = errors.New("forced shutdown: a second shutdown signal cut the guest's power")

// vmdSignalBuffer sizes the signal channel. Two is the meaningful capacity: the
// first SIGTERM starts the drain and the second escalates, and anything beyond
// that is the same request repeated.
const vmdSignalBuffer = 2

// vmdDrainer is the part of *vmhost.Host the signal loop uses, so the loop can
// be tested against a fake that blocks for as long as the test likes.
type vmdDrainer interface {
	Drain(ctx context.Context, timeout time.Duration) error
}

// vmdSignalLoop maps SIGTERM onto the shutdown path.
//
//   - The FIRST SIGTERM means "eject in an orderly way": it runs Drain on its
//     own goroutine, so the loop stays responsive while the guest powers off
//     (which can take the whole drain budget). Drain releases Run itself once
//     the guest is genuinely stopped.
//   - A SECOND SIGTERM arriving while that drain is still in flight is an
//     explicit escalation: it is logged as such and cancels the run context,
//     which unblocks Run and lets teardown force the VMM down. This is a power
//     cut and the log says so — it is offered because a guest that will not
//     power off must not leave the user with no way out.
//
// It returns when ctx is done.
func vmdSignalLoop(ctx context.Context, signals <-chan os.Signal, host vmdDrainer, escalate context.CancelFunc, timeout time.Duration) {
	draining := false
	for {
		select {
		case <-ctx.Done():
			return
		case sig, ok := <-signals:
			if !ok {
				return
			}
			if draining {
				logging.L().Warn("second shutdown signal while draining; forcing the VM down",
					"signal", sig.String())
				escalate()
				return
			}
			draining = true
			logging.L().Info("shutdown signal received; draining the guest",
				"signal", sig.String(), "timeout", timeout)
			go vmdDrain(ctx, host, escalate, timeout)
		}
	}
}

// vmdDrain runs the orderly spin-down for a signal and falls back to releasing
// Run when there is nothing to drain or the drain fails.
func vmdDrain(ctx context.Context, host vmdDrainer, release context.CancelFunc, timeout time.Duration) {
	err := host.Drain(ctx, timeout)
	switch {
	case err == nil:
		return // Drain released Run on its own
	case errors.Is(err, vmhost.ErrNotStarted):
		logging.L().Info("nothing to drain; shutting the holder down")
	default:
		logging.L().Error("drain failed; shutting the holder down", "error", err)
	}
	release()
}

// vmdLogPath returns the holder's log file. A detached holder has no terminal,
// so this is the only place its output can go.
//
// The file is named PER INSTANCE — <stateDir>/vmd-<name>.log — because the
// state dir alone does not separate holders: a cartridge holder is spawned with
// the registry ROOT as its state dir (its own state lives inside a volume it
// has not mounted yet), so every cartridge ever booted would otherwise append
// to one shared vmd.log and interleave. The flat default instance keeps the
// original vmd.log name, so nothing that reads it has to learn a new path.
func vmdLogPath(stateDir, name string) string {
	return filepath.Join(stateDir, vmdLogName(name))
}

// vmdLogName is the holder log file name for an instance.
//
// A name that is not a usable path element falls back to the default file
// rather than being pasted into a path: this runs before the holder exists, so
// there is nothing yet to reject an invalid name, and a log file is not worth a
// directory traversal.
func vmdLogName(name string) string {
	if name == "" || name == config.DefaultInstanceName || instance.ValidName(name) != nil {
		return vmdDefaultLogName
	}
	return vmdLogPrefix + name + vmdLogExt
}

const (
	// vmdDefaultLogName is the holder log of the flat default instance, which
	// has carried this name since holders existed.
	vmdDefaultLogName = "vmd.log"
	// vmdLogPrefix / vmdLogExt bracket a named instance's holder log.
	vmdLogPrefix = "vmd-"
	vmdLogExt    = ".log"
)

// vmdLogRotation caps the holder log. The holder writes to it for as long as
// the VM runs and nothing else ever truncates it, so without a cap it is a file
// that only grows. Rotation happens at OPEN time (see openVMDLog): the writer
// is a detached child process, so this side can act only when it hands over the
// descriptor.
var vmdLogRotation = logging.RotateOptions{
	MaxSize:    vmdLogMaxSizeMB,
	MaxBackups: vmdLogMaxBackups,
	MaxAge:     vmdLogMaxAgeDays,
	Compress:   true,
}

const (
	// vmdLogMaxSizeMB is the size at which the holder log is rotated.
	vmdLogMaxSizeMB = 10
	// vmdLogMaxBackups is how many rotated holder logs are kept.
	vmdLogMaxBackups = 3
	// vmdLogMaxAgeDays is how long a rotated holder log is kept.
	vmdLogMaxAgeDays = 14
)

// openVMDLog opens the holder log for appending, creating it 0600 — it carries
// instance paths and boot detail and is nobody else's business. An oversized
// log is rotated first, so a machine that boots the same instance for months
// does not accumulate one unbounded file.
//
// A rotation failure is logged and not fatal: it costs disk, whereas refusing
// to spawn costs the user their VM.
func openVMDLog(stateDir, name string) (*os.File, error) {
	path := vmdLogPath(stateDir, name)
	if err := logging.RotateIfLarger(path, vmdLogRotation); err != nil {
		logging.L().Warn("could not rotate the holder log", "path", path, "err", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open holder log: %w", err)
	}
	return f, nil
}
