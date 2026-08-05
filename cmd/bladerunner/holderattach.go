package main

// Attaching the terminal to a holder.
//
// Every ordinary way of starting a VM — `br start`, `br up`, `br boot <disk>`,
// `br boot <cartridge>`, `br restore`, `br upgrade`, and the auto-start behind
// a verb that needs a VM — now spawns a `br vmd` holder and then ATTACHES to
// it: it renders the same boot board, waits for the same "ready", prints the
// same summary, and leaves the VM running when it returns. Closing the terminal
// no longer takes the VM with it.
//
// Nothing here talks to the VM. Everything the terminal shows is read from
// artifacts the holder publishes for exactly this purpose:
//
//   - the guest serial console log, tailed into the boot board (unchanged: it
//     was always a file, and the board always read it from one);
//   - the boot-stage file, which is how the boot phase reports that the Incus
//     readiness wait finished — the signal that used to be an in-process return
//     value;
//   - the instance registry entry, which carries the ports actually reserved;
//   - the control socket, which answers "are you there".
//
// The holder's own liveness is the fourth input, and the important one: a
// holder that died during startup must produce an error here rather than a
// silent wait to the timeout, because its log is the only place its reason was
// written.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/stuffbucket/bladerunner/internal/bootstage"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/instance"
	"github.com/stuffbucket/bladerunner/internal/logging"
	"github.com/stuffbucket/bladerunner/internal/ui/board"
	"github.com/stuffbucket/bladerunner/internal/vmhost"
)

// vmStartReadyTimeout bounds how long an attachment waits for a spawned holder
// to publish its readiness. It is the outer budget only: the readiness wait
// itself is bounded by the instance's own --timeout, which is smaller, so this
// expiring means the holder never got as far as reporting anything.
const vmStartReadyTimeout = 15 * time.Minute

// holderPollInterval is how often the attachment re-reads the holder's
// published state. The board tails the console far more often than this; this
// governs only the "has it finished" question.
const holderPollInterval = 500 * time.Millisecond

// holderLogTailLines is how much of a dead holder's log is quoted in the error.
// Enough to carry the failure and its immediate context, short enough to stay
// readable in a terminal.
const holderLogTailLines = 12

// errHolderDetached is the condition of a terminal that stopped watching a
// holder before it was ready — Ctrl+C, or a parent shutting down.
//
// It is a real error VALUE even though a detach is not a failure, and that is
// the point: the wait loops poll, and a nil from the poll means "nothing has
// happened yet, keep going". Reporting a detach as nil made those loops spin
// forever on a terminal the user had already left. Only holderAttachment.finish
// turns it back into the success it is.
var errHolderDetached = errors.New("detached from the starting VM")

// holderAttachOptions tunes what the terminal sees while the holder comes up.
type holderAttachOptions struct {
	// Quiet suppresses the banner, the board and the summary. It is for the
	// auto-start behind a verb that wants a VM in order to do something else
	// (`br shell` on a stopped instance), where the boot is a means and not the
	// output.
	Quiet bool
	// JSON emits the machine-readable start report instead of the human
	// summary, matching `br start --json`.
	JSON bool
	// Timeout overrides vmStartReadyTimeout. Zero uses it.
	Timeout time.Duration
}

// startUnderHolder spawns a holder for spec and watches it come up.
//
// It returns once the instance has reported a terminal boot stage, or with an
// error if the holder died or the outer budget expired. On return the VM is
// running and owned by the holder: this process exiting does not stop it.
func startUnderHolder(ctx context.Context, spec vmhost.Spec, opts holderAttachOptions) error {
	// The holder must NOT inherit this context: it is the process that outlives
	// this command, and binding it to a context this command cancels on exit is
	// precisely the lifetime coupling being removed here. spawnDetached uses
	// context.Background() deliberately; see its doc comment.
	pid, err := spawnHolder(holderSpawn{Spec: spec}) //nolint:contextcheck // the child must outlive this context by design
	if err != nil {
		return err
	}
	a := &holderAttachment{spec: spec, pid: pid, opts: opts, spawnedAt: time.Now()}
	a.announce()
	return a.finish(a.wait(ctx))
}

// holderAttachment is one terminal watching one holder start.
type holderAttachment struct {
	spec      vmhost.Spec
	pid       int
	opts      holderAttachOptions
	spawnedAt time.Time
}

// finish maps the wait's outcome onto the command's exit status. A detach is
// success: the VM is running, which is what was asked for; only this terminal
// stopped watching it.
func (a *holderAttachment) finish(err error) error {
	if errors.Is(err, errHolderDetached) {
		return nil
	}
	return err
}

// announce says what was started, so a user who sees nothing else at least has
// a process to look at.
func (a *holderAttachment) announce() {
	if a.opts.Quiet || a.opts.JSON {
		return
	}
	fmt.Printf("%s Starting VM under holder pid %d\n", subtle("›"), a.pid)
}

// deadline is when this attachment gives up waiting.
func (a *holderAttachment) deadline() time.Time {
	budget := a.opts.Timeout
	if budget <= 0 {
		budget = vmStartReadyTimeout
	}
	return a.spawnedAt.Add(budget)
}

// wait resolves where the instance landed, renders the boot board, and blocks
// until it is up.
func (a *holderAttachment) wait(ctx context.Context) error {
	stateDir, err := a.awaitStateDir(ctx)
	if err != nil {
		return err
	}
	cfg, cfgErr := config.Default(stateDir)
	if cfgErr != nil {
		return fmt.Errorf("resolve the instance's paths: %w", cfgErr)
	}

	stopBoard := a.startBoard(ctx, cfg)
	defer stopBoard()

	stage, err := a.awaitTerminalStage(ctx, stateDir)
	stopBoard()
	if err != nil {
		return err
	}
	a.report(cfg, stateDir, stage)
	return nil
}

// awaitStateDir resolves the directory the instance's published files live in.
//
// For everything but a browsably mounted cartridge that is known up front. A
// cartridge is mounted where MACOS decides — under /Volumes, with a collision
// suffix when a volume of that name is already there — so the only authority is
// the registry entry the holder publishes once it has attached the image.
//
// The entry is looked up under vmhost.RegistryRoot, NOT under the instance's
// own state directory. There is one registry per machine and it lives beside
// the default instance; a disk slot or a custom --state-dir has no `instances/`
// directory of its own, so keying the lookup on it would find nothing.
func (a *holderAttachment) awaitStateDir(ctx context.Context) (string, error) {
	if a.spec.Kind != instance.KindCartridge {
		return a.spec.StateDir, nil
	}
	name := a.instanceName()
	if name == "" {
		return "", errors.New("a cartridge holder must be spawned with a name")
	}
	for {
		if e, err := instance.Read(vmhost.RegistryRoot(), name); err == nil && e.StateDir != "" {
			return e.StateDir, nil
		}
		if err := a.pause(ctx); err != nil {
			return "", err
		}
	}
}

// instanceName is the registry key the holder will publish under, which for a
// cartridge is what the CLI derived from the image path.
func (a *holderAttachment) instanceName() string {
	return holderSpawn{Spec: a.spec}.logName()
}

// pause sleeps one poll interval, and is where every wait loop learns that the
// terminal went away, the holder died, or the budget ran out.
func (a *holderAttachment) pause(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return a.detached()
	case <-time.After(holderPollInterval):
	}
	if !instance.ProcessAlive(a.pid) {
		return a.holderDied()
	}
	if time.Now().After(a.deadline()) {
		return fmt.Errorf("timed out after %v waiting for the VM to start; it may still be coming up — check 'br instances' and %s",
			time.Since(a.spawnedAt).Round(time.Second), a.logPath())
	}
	return nil
}

// awaitTerminalStage blocks until the holder reports how the boot ended.
//
// A stage written BEFORE this attachment spawned its holder is ignored. The
// boot-stage file is cleared on teardown, but a holder that was killed rather
// than drained leaves the last one behind, and treating that corpse as this
// boot's result would report a VM ready before it had booted.
func (a *holderAttachment) awaitTerminalStage(ctx context.Context, stateDir string) (bootstage.Stage, error) {
	for {
		if s, ok := bootstage.Read(stateDir); ok && a.isThisBoot(s) && isTerminalBootStage(s.Stage) {
			return s.Stage, nil
		}
		if err := a.pause(ctx); err != nil {
			return "", err
		}
	}
}

// isThisBoot reports whether a published stage belongs to the holder this
// attachment spawned rather than to a previous run of the same instance.
func (a *holderAttachment) isThisBoot(s bootstage.State) bool {
	return !s.UpdatedAt.Before(a.spawnedAt)
}

// isTerminalBootStage reports whether the boot phase has finished, either way.
// A degraded boot (Failed) still leaves a VM the user can go and look at, which
// is why it ends the wait rather than aborting it.
func isTerminalBootStage(s bootstage.Stage) bool {
	return s == bootstage.Ready || s == bootstage.Failed
}

// detached reports that the terminal stopped watching. The VM is deliberately
// left alone.
func (a *holderAttachment) detached() error {
	if !a.opts.Quiet && !a.opts.JSON {
		fmt.Println()
		fmt.Printf("%s The VM keeps running — it is owned by holder pid %d, not by this terminal.\n",
			subtle("Detached."), a.pid)
		fmt.Printf("  %s %s\n", key("Stop it:"), command(a.stopCommand()))
		fmt.Printf("  %s %s\n", key("Watch it:"), command("br status"))
	}
	return errHolderDetached
}

// stopCommand is what shuts this particular instance down, named for the
// instance rather than assuming the default one.
func (a *holderAttachment) stopCommand() string {
	if name := a.instanceName(); name != "" {
		return "br stop --instance " + name
	}
	return "br stop"
}

// holderDied turns "the process is gone" into an error a user can act on, by
// quoting the end of the log the holder wrote its reason to. Without this the
// failure is invisible: a detached holder has no terminal, so nothing it
// printed reached the one the user is looking at.
func (a *holderAttachment) holderDied() error {
	path := a.logPath()
	tail := tailFile(path, holderLogTailLines)
	if tail == "" {
		return fmt.Errorf("the VM holder (pid %d) exited during startup; its log is %s", a.pid, path)
	}
	return fmt.Errorf("the VM holder (pid %d) exited during startup:\n%s\n  (full log: %s)", a.pid, tail, path)
}

// logPath is the holder's log file for this instance.
func (a *holderAttachment) logPath() string {
	return vmdLogPath(a.spec.StateDir, a.instanceName())
}

// tailFile returns the last n lines of a file, indented for quoting inside an
// error message, or "" when it cannot be read. Best effort by design: it is
// used to enrich a failure, never to produce one.
func tailFile(path string, n int) string {
	blob, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(blob), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	if len(lines) == 1 && lines[0] == "" {
		return ""
	}
	return "    " + strings.Join(lines, "\n    ")
}

// startBoard renders the buildx-style boot board against the console log the
// holder is writing, and returns a function that tears it down. It is
// idempotent, so the caller can stop it early and still defer it.
//
// Two of the board's four stages — "VM running" and "Incus API ready" — are
// reported by the RUNNER, which is in the holder's process, so the in-process
// progress adapter startBootBoard returns is useless here and is dropped. The
// boot-stage file carries the same transitions across the process boundary;
// driveBoardFromBootStage replays them.
func (a *holderAttachment) startBoard(ctx context.Context, cfg *config.Config) func() {
	if a.opts.Quiet || a.opts.JSON {
		return func() {}
	}
	brd, _, tailCancel := startBootBoard(ctx, cfg)
	boardCtx, boardCancel := context.WithCancel(ctx)
	if brd != nil {
		go a.driveBoardFromBootStage(boardCtx, brd, cfg.VMDir)
	}
	stopped := false
	return func() {
		if stopped {
			return
		}
		stopped = true
		boardCancel()
		tailCancel()
		if brd != nil {
			brd.Stop()
		}
	}
}

// driveBoardFromBootStage replays the holder's published boot phase onto the
// board's runner-fed stages. It is the exact inverse of the mapping the Host
// publishes with (vmhost's bootStageProgress), so the attached board shows the
// same four stages advancing that an in-process start showed.
func (a *holderAttachment) driveBoardFromBootStage(ctx context.Context, brd *board.Board, stateDir string) {
	seen := map[bootstage.Stage]bool{}
	step := func(stage bootstage.Stage, apply func()) {
		if seen[stage] {
			return
		}
		seen[stage] = true
		apply()
	}
	for {
		if s, ok := bootstage.Read(stateDir); ok && a.isThisBoot(s) {
			switch s.Stage {
			case bootstage.Boot:
				step(bootstage.Boot, func() { brd.Begin(boardStageVMBoot) })
			case bootstage.Setup:
				step(bootstage.Boot, func() { brd.Begin(boardStageVMBoot) })
				step(bootstage.Setup, func() { brd.Complete(boardStageVMBoot) })
			case bootstage.Incus:
				step(bootstage.Setup, func() { brd.Complete(boardStageVMBoot) })
				step(bootstage.Incus, func() { brd.Begin(boardStageIncusWait) })
			case bootstage.Ready:
				step(bootstage.Setup, func() { brd.Complete(boardStageVMBoot) })
				step(bootstage.Ready, func() { brd.Complete(boardStageIncusWait) })
				return
			case bootstage.Failed:
				step(bootstage.Failed, func() {
					brd.Fail(boardStageIncusWait, errors.New("the guest did not reach Incus readiness"))
				})
				return
			default:
				// Connect is driven by the console tailer (it is the ssh stage
				// the board already parses), and every shutdown-phase stage
				// belongs to a drain, which this attachment never watches.
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(holderPollInterval):
		}
	}
}

// report prints the running summary for an instance that has finished booting.
//
// The ports come from the registry entry rather than from the config resolved
// here: they are ASSIGNED at boot (an additional instance falls back to
// ephemeral ports), so the defaults this process computed are only right for
// the first instance on the machine. The entry is read from the one registry —
// see awaitStateDir — not from the instance's own directory.
func (a *holderAttachment) report(cfg *config.Config, stateDir string, stage bootstage.Stage) {
	if a.opts.Quiet {
		return
	}
	entry, err := instance.Read(vmhost.RegistryRoot(), a.instanceNameFor(stateDir))
	if err != nil {
		logging.L().Debug("no registry entry for the started instance", "err", err)
	}
	if entry.Ports != (instance.Ports{}) {
		cfg.AssignPorts(config.PortAssignment{
			SSH: entry.Ports.SSH, API: entry.Ports.API, Web: entry.Ports.Web,
			OIDC: entry.Ports.OIDC, NTP: entry.Ports.NTP,
		})
	}
	endpoint := config.APIEndpoint(cfg.LocalAPIPort)
	bootErr := bootStageError(stage, stateDir)
	protection := protectionReportFor(entry.Kind, entry.UnmountProtection)

	if a.opts.JSON {
		_ = startReportJSON(cfg, endpoint, bootErr, protection)
		return
	}
	printRunningSummary(cfg, endpoint, bootErr)
	_ = writeUnprotectedCartridge(os.Stdout, protection)
	printHolderNotice(a.pid, a.stopCommand())
}

// instanceNameFor is the registry key to look the started instance up under.
// A cartridge was spawned with an explicit name; everything else is keyed on
// the basename of its own state directory, exactly as the Host derives it.
func (a *holderAttachment) instanceNameFor(stateDir string) string {
	if name := a.instanceName(); name != "" {
		return name
	}
	return config.InstanceNameFor(stateDir)
}

// bootStageError renders a degraded boot as the error the summary reports. A
// separate process cannot see the readiness wait's actual error, so it names
// the console log — which is where the answer is, and where the in-process
// summary pointed too.
func bootStageError(stage bootstage.Stage, stateDir string) error {
	if stage != bootstage.Failed {
		return nil
	}
	return fmt.Errorf("the guest did not reach Incus readiness; see the console log under %s", stateDir)
}

// printHolderNotice states the thing that is new about this run: the VM is not
// this terminal's child, so leaving does not stop it.
func printHolderNotice(pid int, stopCmd string) {
	fmt.Printf("  %s %s\n", key("Holder:"), subtle(fmt.Sprintf("pid %d — the VM keeps running after this command exits", pid)))
	fmt.Printf("  %s %s\n", key("Stop:"), command(stopCmd))
	fmt.Println()
}

// alreadyRunningAt reports the "this instance is already up" refusal, or nil.
//
// The check moves to the CLI because the Host now runs in another process: its
// own ErrAlreadyRunning would be written to a holder log nobody is reading,
// and the terminal would show a spawned pid followed by a wait that could only
// end in a timeout.
//
// It asks the liveness ladder rather than pinging. A holder that is alive but
// wedged replies to nothing, so a ping-shaped gate would let a second holder
// through — and the second one would then fail on the start lock the first one
// still owns, after the terminal had already reported a spawn.
func alreadyRunningAt(stateDir string) error {
	if !instanceHeld(stateDir) {
		return nil
	}
	return explainHostError(fmt.Errorf("%w", vmhost.ErrAlreadyRunning))
}
