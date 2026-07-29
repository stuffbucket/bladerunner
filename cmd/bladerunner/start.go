package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/boot"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/control"
	"github.com/stuffbucket/bladerunner/internal/instance"
	"github.com/stuffbucket/bladerunner/internal/ui"
	"github.com/stuffbucket/bladerunner/internal/ui/board"
	"github.com/stuffbucket/bladerunner/internal/vm"
	"github.com/stuffbucket/bladerunner/internal/vmhost"
	"golang.org/x/term"
)

var startFlags struct {
	cpus        uint
	memory      uint64
	disk        int
	gui         bool
	stateDir    string
	imageURL    string
	imagePath   string
	hostedImage bool
	debianImage bool
	timeout     time.Duration
	noNested    bool
	restoreFrom string
}

// startFlagNames lists every flag `br start` owns, so a Spec can record which
// of them the user actually set. It is enumerated (rather than walked with
// pflag) because runStart is also reached from `br up`, `br boot`, `br restore`
// and `br upgrade`, whose commands carry a different flag set; a name the
// command does not define simply reports "not changed", exactly as before.
var startFlagNames = []string{
	"cpus", "memory", "disk", "gui", "state-dir", "image-url", "image-path",
	"hosted-image", "debian-image", "timeout", "no-nested-virt", "restore",
}

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start a new VM instance",
	Long: `Start a new Incus VM instance. If no VM exists, one will be created
with cloud-init provisioning.

The VM is owned by a small holder process, not by this command. Start prints
the boot progress and returns once the guest is up, and the VM KEEPS RUNNING:
closing the terminal does not stop it. Press Ctrl+C while it is coming up and
this command detaches — the VM carries on booting. Shut it down with 'br stop'.

  --gui is the exception. A console window has to be opened by the process that
  owns the main thread, so a GUI boot runs in the FOREGROUND and the VM stops
  when this command does, exactly as it always has. The same applies when the
  menubar's "show console" setting is on.`,
	RunE: runStart,
}

func init() {
	f := startCmd.Flags()
	f.UintVar(&startFlags.cpus, "cpus", config.DefaultCPUs, "Number of CPUs")
	f.Uint64Var(&startFlags.memory, "memory", config.DefaultMemoryGiB, "Memory in GiB")
	f.IntVar(&startFlags.disk, "disk", config.DefaultDiskSizeGiB, "Disk size in GiB")
	f.BoolVar(&startFlags.gui, "gui", false, "Open GUI console window")
	f.StringVar(&startFlags.stateDir, "state-dir", "", "State directory (default: ~/.local/state/bladerunner)")
	f.StringVar(&startFlags.imageURL, "image-url", "", "Base image URL")
	f.StringVar(&startFlags.imagePath, "image-path", "", "Local base image path")
	f.BoolVar(&startFlags.hostedImage, "hosted-image", false, "Force the pre-baked hosted guest image (guest-image-latest release); the default already resolves to it (also settable via BLADERUNNER_FORCE_HOSTED_IMAGE=1)")
	f.BoolVar(&startFlags.debianImage, "debian-image", false, "Escape hatch: force the Debian Trixie genericcloud + cloud-init path instead of the pre-baked default (also settable via BLADERUNNER_FORCE_DEBIAN_IMAGE=1)")
	f.DurationVar(&startFlags.timeout, "timeout", config.DefaultTimeout, "How long to wait for the guest's Incus API to come up and authorize this client")
	f.BoolVar(&startFlags.noNested, "no-nested-virt", false, "Disable nested virtualization even if the host supports it (Incus VMs will be unavailable)")
	f.StringVar(&startFlags.restoreFrom, "restore", "", "Restore the guest from a saved-state file (see 'br save') instead of cold-booting")
}

// nestedVirtBanner describes whether the guest's Incus will be able to run VMs
// (not just containers), for the start banner. The host capability is known up
// front, before the VM is configured.
func nestedVirtBanner() string {
	switch {
	case startFlags.noNested:
		return warning("disabled (--no-nested-virt)")
	case vm.NestedVirtualizationSupported():
		return success("enabled (nested virtualization)")
	default:
		return subtle("unsupported — containers only (host lacks nested virtualization)")
	}
}

// runStart brings one instance up.
//
// # Where the VM actually runs
//
// Headless — the ordinary case — the VM is run by a HOLDER: a detached
// `br vmd` that owns it and outlives this command. This function spawns that
// holder, attaches the terminal to it (the same banner, the same boot board,
// the same running summary), and returns once the guest is up, leaving the VM
// running. Closing the terminal, or Ctrl+C, no longer stops the VM. That is
// goal 1 of the cartridge runtime, and until now only `br watch` and the
// menubar reached it.
//
// # Except with --gui
//
// A GUI boot stays in the FOREGROUND, and it has to: vz.StartGraphicApplication
// takes the main thread of this process and never gives it back, so the window
// belongs to whichever process called it. Running it under a holder would put
// the window on a process with no terminal and no relationship to the one the
// user typed in, and closing that window is the gesture that ends the VM. So
// `br start --gui`, `br boot --gui` and a start whose persisted Settings ask
// for a console keep exactly the behavior they had, terminal-bound lifetime
// included.
func runStart(cmd *cobra.Command, args []string) error {
	spec := buildStartSpec(cmd)
	// Refuse before spawning anything: a slot the registry cannot accept would
	// otherwise boot a VM that never appears in 'br instances'.
	if err := registrableSlotName(spec); err != nil {
		return err
	}
	if guiRequested(spec) {
		return runStartForeground(cmd, args, spec)
	}
	return runStartUnderHolder(spec)
}

// guiRequested reports whether this start opens a console window, as far as
// this process can know before a Host has resolved the config.
//
// A boot carries the decision pre-resolved in its overrides. A plain start
// carries it only when the user passed --gui; otherwise it comes from the
// persisted Settings, which the menubar's "show console" switch writes and
// which this reads for the same reason the Host does. A cartridge whose own
// manifest asks for a GUI is NOT visible here — the manifest lives inside an
// image nothing has attached yet — so that one boot runs its window under the
// holder, exactly as an inserted cartridge already does.
func guiRequested(spec vmhost.Spec) bool {
	if spec.GUIRequested() {
		return true
	}
	if spec.Driven {
		// A driven spec asserts every flag, so the answer above was complete.
		return false
	}
	settings, err := config.LoadSettings(config.DefaultStateDir())
	if err != nil {
		return false
	}
	return settings.ShowConsole
}

// runStartUnderHolder spawns the holder that will own this instance and watches
// it come up.
func runStartUnderHolder(spec vmhost.Spec) error {
	ctx, stop := signalContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if spec.StateDir == "" {
		spec.StateDir = config.DefaultStateDir()
	}
	// The "it is already up" refusal has to be made HERE now. The Host still
	// makes it, but it makes it inside a detached process whose only output is
	// a log file, so the terminal would show a spawned pid and then wait for a
	// readiness that could never arrive. A cartridge is exempt: it is checked
	// by image in ensureCartridgeBootable, because its state dir is the
	// registry root and says nothing about the cartridge.
	if spec.Kind != instance.KindCartridge {
		if err := alreadyRunningAt(spec.StateDir); err != nil {
			return err
		}
	}
	return startUnderHolder(ctx, spec, holderAttachOptions{
		JSON:    jsonOutput,
		Timeout: attachBudget(spec),
	})
}

// attachBudgetSlack is how much longer than the instance's own readiness budget
// the terminal keeps watching. It covers everything the readiness wait does not
// — attaching a cartridge, materializing a disk, downloading a base image —
// so the outer budget never expires first and blames the wrong thing.
const attachBudgetSlack = 10 * time.Minute

// attachBudget bounds the terminal's watch on a starting holder.
func attachBudget(spec vmhost.Spec) time.Duration {
	if spec.Overrides.Timeout <= 0 {
		return 0 // the attachment's own default
	}
	return spec.Overrides.Timeout + attachBudgetSlack
}

// runStartForeground runs the VM in THIS process, which is what a GUI boot
// requires. It is the pre-holder behavior, unchanged: the VM's lifetime is this
// command's lifetime.
func runStartForeground(_ *cobra.Command, _ []string, spec vmhost.Spec) error {
	ctx, stop := signalContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Safety net for a cartridge boot that never reaches the Host (a rejected
	// spec, say): the mount must not be stranded. Once ownership is handed over
	// below this is a no-op.
	defer detachBootCartridge()

	host, err := vmhost.New(spec)
	if err != nil {
		return err
	}
	host.AdoptCartridge(takeBootCartridge())

	obs := &cliObserver{json: jsonOutput, host: host}
	defer obs.close()
	host.SetObserver(obs)

	return explainHostError(host.Run(ctx))
}

// errSignaled is the cause signalContext records, so a caller can ask "was this
// process killed?" with errors.Is instead of parsing a string.
var errSignaled = errors.New("received signal")

// signalContext is signal.NotifyContext with the SIGNAL RECORDED AS THE CAUSE.
//
// # Why not just use signal.NotifyContext
//
// Because on the Go version this project pins it does not do this. Naming the
// signal in context.Cause landed in the standard library AFTER go 1.25 (a
// go1.26 toolchain reports "terminated signal received"; go1.25.6 — what go.mod
// asks for, what the CI container runs and what releases are built with —
// reports a bare "context canceled"). Relying on it would mean the diagnosis
// below works on a developer's newer local toolchain and silently vanishes in
// CI and in shipped builds, which is worse than not having it. Ten lines here
// make it true everywhere.
//
// # Why it is worth those ten lines
//
// Everything downstream of this context — the Incus readiness wait above all —
// can only observe "context canceled" when it is cut short, and that is
// indistinguishable from the wait's own budget expiring. A cartridge boot killed
// by an outer process timeout therefore read exactly like a guest that booted
// too slowly, and telling the two apart cost a full VM run. With a cause the
// same log line reads "canceled after 2m26s ... (cause: received signal:
// terminated)" and the question is already answered. vmhost.cancelReason is what
// renders it.
//
// The returned stop function releases the signal registration and cancels the
// context, exactly as NotifyContext's does.
func signalContext(parent context.Context, sigs ...os.Signal) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, sigs...)
	go func() {
		select {
		case sig := <-ch:
			// The cause is set BEFORE Done closes (cancel does both, in that
			// order), so anyone woken by Done always sees it. No race, no wait.
			cancel(fmt.Errorf("%w: %s", errSignaled, sig))
		case <-ctx.Done():
		}
	}()
	return ctx, func() {
		signal.Stop(ch)
		cancel(nil)
	}
}

// A Host declines to start for two reasons that are not really errors so much
// as facts about the machine: someone else already holds this instance
// (control.ErrInstanceLocked, taken before the socket dance) or it is already
// running (vmhost.ErrAlreadyRunning, taken when the socket answers). Both are
// sentinels, and until this existed nothing matched them — so the friendly path
// they were declared for did not exist and the user got the raw wrapped text.
//
// explainHostError is where they are matched. It appends a hint and keeps the
// sentinel wrapped, so `errors.Is` still works for any caller above.
func explainHostError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, control.ErrInstanceLocked):
		return fmt.Errorf("%w\n%s", err, instanceLockedHint(err))
	case errors.Is(err, vmhost.ErrAlreadyRunning):
		return fmt.Errorf("%w\n%s", err, alreadyRunningHint)
	default:
		return err
	}
}

// alreadyRunningHint is what to do about an instance whose control socket
// already answers.
const alreadyRunningHint = "  it is already up: 'br instances' lists it, 'br shell' gets into it, 'br stop' shuts it down"

// instanceLockedHint names the process holding the instance, because "another
// bladerunner process" is not something a user can act on and a PID is: it is
// what `br instances` shows and what a kill needs when a holder has wedged.
func instanceLockedHint(err error) string {
	if pid, ok := lockHolderPID(err); ok {
		return fmt.Sprintf("  process %d holds it: 'br instances' shows what it is running, 'br stop' releases it", pid)
	}
	return "  'br instances' shows which process holds it, 'br stop' releases it"
}

// lockHolderPIDMarker is how internal/control names the holder when it could
// read the lock file: "...: pid 1234 holds /path/control.lock".
const lockHolderPIDMarker = "pid "

// lockHolderPID digs the holding process's PID out of a wrapped
// control.ErrInstanceLocked.
//
// It reads the message because the sentinel carries no typed field for the PID
// (follow-up: make it a struct error and delete this). That is why it is only a
// HINT: the contended branch names no holder at all, so a caller must cope with
// (0, false), and a message that changes shape degrades the hint rather than
// the error.
func lockHolderPID(err error) (int, bool) {
	_, digits, found := strings.Cut(err.Error(), lockHolderPIDMarker)
	if !found {
		return 0, false
	}
	end := 0
	for end < len(digits) && digits[end] >= '0' && digits[end] <= '9' {
		end++
	}
	pid, convErr := strconv.Atoi(digits[:end])
	if convErr != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// buildStartSpec turns the `start` flags — plus the disk manifest or open
// cartridge a `br boot` stashed — into the serializable description of one
// instance that vmhost runs.
func buildStartSpec(cmd *cobra.Command) vmhost.Spec {
	spec := vmhost.Spec{
		Kind:          instance.KindFlat,
		StateDir:      startFlags.stateDir,
		Manifest:      bootManifest,
		RestoreFrom:   startFlags.restoreFrom,
		BinaryVersion: version,
		ChangedFlags:  changedStartFlags(cmd),
		Overrides: vmhost.Overrides{
			CPUs:         startFlags.cpus,
			MemoryGiB:    startFlags.memory,
			DiskSizeGiB:  startFlags.disk,
			GUI:          startFlags.gui,
			ImageURL:     startFlags.imageURL,
			ImagePath:    startFlags.imagePath,
			HostedImage:  startFlags.hostedImage,
			DebianImage:  startFlags.debianImage,
			Timeout:      startFlags.timeout,
			NoNestedVirt: startFlags.noNested,
		},
		// A boot/cartridge start stuffed pre-resolved precedence into the flags
		// (including a --headless override of a GUI manifest), so every flag is
		// applied verbatim; a plain start applies only what the user changed.
		Driven: bootManifest != nil || bootCartridge.mountpoint != "",
	}
	// Spec.Name is deliberately left empty: the Host derives it with
	// config.InstanceName (the slot directory's basename), which is exactly the
	// disk/cartridge name and needs no second validation pass. Setting it here
	// would subject boots that work today to instance.ValidName's 64-character
	// bound, which disk.ValidName does not impose.
	if bootManifest != nil {
		spec.Kind = instance.KindDisk
	}
	if opened := bootCartridge.opened; opened != nil {
		spec.Kind = instance.KindCartridge
		spec.CartridgePath = opened.SourcePath
		spec.Mountpoint = opened.Mountpoint()
	}
	return spec
}

// changedStartFlags reports the `start` flags the user explicitly set on cmd.
func changedStartFlags(cmd *cobra.Command) []string {
	if cmd == nil {
		return nil
	}
	changed := make([]string, 0, len(startFlagNames))
	for _, name := range startFlagNames {
		if cmd.Flags().Changed(name) {
			changed = append(changed, name)
		}
	}
	return changed
}

// cliObserver is the terminal front end for a vmhost.Host: the start banner,
// the buildx-style boot board, and the running summary (human or --json). It
// holds every piece of rendering state that used to be local to runStart.
type cliObserver struct {
	json bool
	// host is the instance being reported on. It is read only for state the
	// summary needs and the config cannot carry — the cartridge eject veto,
	// which is decided by the Host itself.
	host       *vmhost.Host
	board      *board.Board
	tailCancel context.CancelFunc
}

// protection reports the eject veto of the instance being started, or nil when
// it has no cartridge to protect.
func (o *cliObserver) protection() *unmountProtectionReport {
	if o.host == nil {
		return nil
	}
	info := o.host.Info()
	return protectionReportFor(info.Kind, info.UnmountProtection)
}

// Resolved prints the pre-boot banner.
func (o *cliObserver) Resolved(cfg *config.Config) {
	if o.json {
		return
	}
	fmt.Println(title("Starting Bladerunner VM..."))
	fmt.Printf("  %s %s\n", key("Name:"), value(cfg.Name))
	fmt.Printf("  %s %d\n", key("CPUs:"), cfg.CPUs)
	fmt.Printf("  %s %d GiB\n", key("Memory:"), cfg.MemoryGiB)
	fmt.Printf("  %s %s\n", key("Arch:"), value(runtime.GOARCH))
	fmt.Printf("  %s %s\n", key("Incus VMs:"), nestedVirtBanner())
	fmt.Println()
}

// Progress builds the buildx-style boot board when stderr is a TTY. It shows
// stage state on top and a live tail of the guest serial console underneath.
// Non-TTY callers (CI, log capture) still get plain slog output via the noop
// board path. In --json mode we skip it entirely so the only stdout output is
// the final JSON report.
func (o *cliObserver) Progress(ctx context.Context, cfg *config.Config) vm.Progress {
	if o.json {
		return nil
	}
	brd, prog, tailCancel := startBootBoard(ctx, cfg)
	o.board, o.tailCancel = brd, tailCancel
	return prog
}

// Failed tears the board down when the VM never started.
func (o *cliObserver) Failed(error) {
	if o.board != nil {
		o.board.Stop()
	}
}

// Started reports a GUI-mode boot, which cannot wait for Incus first: the macOS
// event loop must take the main thread immediately, so success is not yet known.
func (o *cliObserver) Started(cfg *config.Config, endpoint string) {
	o.report(cfg, endpoint, nil)
}

// Ready reports a headless boot once the Incus readiness wait has finished.
func (o *cliObserver) Ready(cfg *config.Config, endpoint string, bootErr error) {
	o.report(cfg, endpoint, bootErr)
}

// report emits the running summary as JSON or human text, after tearing down
// the boot board.
func (o *cliObserver) report(cfg *config.Config, endpoint string, bootErr error) {
	if o.board != nil {
		o.board.Stop()
		o.tailCancel()
	}
	if o.json {
		_ = startReportJSON(cfg, endpoint, bootErr, o.protection())
		return
	}
	printRunningSummary(cfg, endpoint, bootErr)
	_ = writeUnprotectedCartridge(os.Stdout, o.protection())
}

// Waiting announces what the foreground is about to block on.
func (o *cliObserver) Waiting(gui bool) {
	if o.json {
		return
	}
	if gui {
		fmt.Println(subtle("Opening GUI window (runs on main thread)..."))
		return
	}
	fmt.Println(subtle("Headless mode. Press Ctrl+C to stop."))
}

// Stopping announces the shutdown.
func (o *cliObserver) Stopping() {
	if !o.json {
		fmt.Println(subtle("\nShutting down..."))
	}
}

// TeardownWarning surfaces a failed teardown step. Only the cartridge step is
// worth a user-visible line: everything else is either logged or harmless.
//
// The error is printed unprefixed because that step now covers two very
// different failures — the detach, and the --persist write-back — and the
// write-back's message already says what happened to which file. Labeling it
// "detach cartridge" would have told a user their changes were lost to a detach
// that in fact succeeded.
func (o *cliObserver) TeardownWarning(step string, err error) {
	if step == vmhost.StepCartridge && !o.json {
		fmt.Printf("%s cartridge: %v\n", warning("⚠"), err)
	}
}

// close stops the console tailer if the board never got torn down (a start that
// failed before any report).
func (o *cliObserver) close() {
	if o.tailCancel != nil {
		o.tailCancel()
	}
}

// startReportJSON emits a one-line JSON object describing the running VM, used
// by `br start --json`. The process keeps running afterward (start is a
// foreground server); agents read this single object to learn the endpoints.
func startReportJSON(cfg *config.Config, endpoint string, bootErr error, protection *unmountProtectionReport) error {
	r := map[string]any{
		jsonFieldStatus: "running",
		"ssh_addr":      fmt.Sprintf("localhost:%d", cfg.LocalSSHPort),
		"api":           endpoint,
		"log":           cfg.LogPath,
	}
	if protection != nil {
		r["unmount_protection"] = protection
	}
	if bootErr != nil {
		r[jsonFieldStatus] = "running-degraded"
		r["boot_error"] = bootErr.Error()
		r["console_log"] = cfg.ConsoleLogPath
	}
	return emitJSON(r)
}

func printRunningSummary(cfg *config.Config, endpoint string, bootErr error) {
	fmt.Println()
	if bootErr == nil {
		fmt.Println(success("✓ VM is running"))
	} else {
		fmt.Println(warning(bootSummaryHeadline(bootErr)))
		fmt.Printf("  %s %v\n", key("Reason:"), bootErr)
		fmt.Printf("  %s %s\n", key("Console:"), value(cfg.ConsoleLogPath))
		fmt.Printf("  %s %s\n", key("Hint:"), subtle(bootSummaryHint(bootErr)))
	}
	fmt.Printf("  %s %s\n", key("SSH:"), command("br ssh"))
	fmt.Printf("  %s %s\n", key("Shell:"), command("br shell"))
	fmt.Printf("  %s %s\n", key("API:"), value(endpoint))
	fmt.Println()
}

// bootSummaryHeadline and bootSummaryHint say which of the two boot failures
// happened, because they call for opposite responses and the summary used to
// describe both as the first one.
//
// A boot that ran out of budget left a VM up that the user can go and inspect.
// A boot that was INTERRUPTED — Ctrl-C, `br stop`, a killed process group — is
// already shutting down: "VM is running" is false by the time it is printed,
// the console log is not evidence of anything wrong, and telling the user to
// wait for cloud-init sends them after a guest that is being powered off.
func bootSummaryHeadline(bootErr error) string {
	if errors.Is(bootErr, context.Canceled) {
		return "⚠ Boot was interrupted before the guest was ready; shutting down"
	}
	return "⚠ VM is running but the guest did not finish booting"
}

func bootSummaryHint(bootErr error) string {
	if errors.Is(bootErr, context.Canceled) {
		return "Nothing is wrong with the guest — it was still coming up when the boot was stopped. Boot it again to finish."
	}
	return "`br shell` and `br ssh` will fail until cloud-init completes."
}

// Board stage IDs (kept as constants so they're referenced consistently across
// the stage list, the runner-stage mapping, and the console tailer).
const (
	boardStageVMBoot    = "vm-boot"
	boardStageCloudInit = "cloud-init"
	boardStageSSH       = "ssh"
	boardStageIncusWait = "incus-wait"
)

// startBootBoard constructs the split-view boot board, wires it into the
// runner as the progress reporter, and starts a console.log tailer that
// feeds raw lines into the tail panel and advances stage state from parsed
// cloud-init / ssh markers. Returns the board (nil when stderr is not a
// TTY) and a cancel function for the tailer goroutine.
func startBootBoard(ctx context.Context, cfg *config.Config) (*board.Board, vm.Progress, context.CancelFunc) {
	if !term.IsTerminal(int(os.Stderr.Fd())) {
		return nil, nil, func() {}
	}
	stages := []board.Stage{
		{ID: boardStageVMBoot, Label: "VM running"},
		{ID: boardStageCloudInit, Label: "cloud-init complete"},
		{ID: boardStageSSH, Label: "SSH ready"},
		{ID: boardStageIncusWait, Label: "Incus API ready"},
	}
	brd := board.New(stages, board.Options{
		Out:            os.Stderr,
		Title:          ui.Title("Bladerunner boot"),
		ConsoleLogPath: cfg.ConsoleLogPath,
	})
	brd.Start()

	tailCtx, cancel := context.WithCancel(ctx)
	go tailConsoleIntoBoard(tailCtx, brd, cfg.ConsoleLogPath)
	return brd, newBoardAdapter(brd), cancel
}

// boardAdapter maps the runner's stage IDs (vm.StageVMBoot, vm.StageIncusWait)
// onto the board's stage IDs. Stages unknown to the board are silently
// dropped so the runner can introduce new stages without breaking older UIs.
type boardAdapter struct{ b *board.Board }

func newBoardAdapter(b *board.Board) *boardAdapter { return &boardAdapter{b: b} }

func (a *boardAdapter) Begin(stage, _ string, _ time.Duration) {
	if id := mapRunnerStage(stage); id != "" {
		a.b.Begin(id)
	}
}

func (a *boardAdapter) Substatus(stage, msg string) {
	if id := mapRunnerStage(stage); id != "" {
		a.b.Substatus(id, msg)
	}
}

func (a *boardAdapter) Done(stage string) {
	if id := mapRunnerStage(stage); id != "" {
		a.b.Complete(id)
	}
}

func (a *boardAdapter) Fail(stage string, err error) {
	if id := mapRunnerStage(stage); id != "" {
		a.b.Fail(id, err)
	}
}

func mapRunnerStage(s string) string {
	switch s {
	case vm.StageVMBoot:
		return boardStageVMBoot
	case vm.StageIncusWait:
		return boardStageIncusWait
	}
	return ""
}

const consoleTailPollInterval = 250 * time.Millisecond

// tailConsoleIntoBoard streams the guest serial console into the board's
// tail panel and advances the cloud-init / ssh stages from the parsed boot
// status. The kernel-boot transition is implicit (it happens before
// cloud-init starts running).
func tailConsoleIntoBoard(ctx context.Context, b *board.Board, path string) {
	var seenKernel, seenCIBegin, seenCIDone, seenCIFail, seenSSH bool
	for ev := range boot.WatchEvents(ctx, path, boot.WatchOptions{
		PollInterval: consoleTailPollInterval,
		FromEnd:      true,
	}) {
		b.AppendLog(ev.Line)
		if ev.Status.KernelBooted && !seenKernel {
			seenKernel = true
		}
		if (ev.Status.KernelBooted || ev.Status.SystemdReached) && !seenCIBegin {
			seenCIBegin = true
			b.Begin(boardStageCloudInit)
		}
		if ev.Status.CloudInitFailed && !seenCIFail {
			seenCIFail = true
			b.Fail(boardStageCloudInit, fmt.Errorf("cloud-init reported failure (see console.log)"))
		}
		if ev.Status.CloudInitDone && !seenCIDone {
			seenCIDone = true
			b.Complete(boardStageCloudInit)
			b.Begin(boardStageSSH)
		}
		if ev.Status.SSHReady && !seenSSH {
			seenSSH = true
			b.Complete(boardStageSSH)
		}
	}
}
