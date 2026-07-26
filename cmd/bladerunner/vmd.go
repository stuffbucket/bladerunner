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

	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()

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

	return host.Run(ctx)
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

// vmdLogPath returns the holder's log file: <stateDir>/vmd.log. A detached
// holder has no terminal, so this is the only place its output can go.
func vmdLogPath(stateDir string) string {
	return filepath.Join(stateDir, vmdLogName)
}

// vmdLogName is the holder's log file name.
const vmdLogName = "vmd.log"

// openVMDLog opens the holder log for appending, creating it 0600 — it carries
// instance paths and boot detail and is nobody else's business.
func openVMDLog(stateDir string) (*os.File, error) {
	f, err := os.OpenFile(vmdLogPath(stateDir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open holder log: %w", err)
	}
	return f, nil
}
