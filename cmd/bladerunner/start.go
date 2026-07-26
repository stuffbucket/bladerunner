package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/boot"
	"github.com/stuffbucket/bladerunner/internal/config"
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
with cloud-init provisioning.`,
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
	f.DurationVar(&startFlags.timeout, "timeout", config.DefaultTimeout, "Wait timeout for Incus")
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

// runStart is the CLI wrapper over internal/vmhost: it turns the flags (plus
// whatever `br boot` stashed) into a vmhost.Spec, installs the terminal front
// end, and hands the whole VM lifecycle to the Host. Everything that owns a
// resource lives in vmhost so a holder process — which cannot import package
// main — can run the exact same sequence.
func runStart(cmd *cobra.Command, _ []string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Safety net for a cartridge boot that never reaches the Host (a rejected
	// spec, say): the mount must not be stranded. Once ownership is handed over
	// below this is a no-op.
	defer detachBootCartridge()

	host, err := vmhost.New(buildStartSpec(cmd))
	if err != nil {
		return err
	}
	host.AdoptCartridge(takeBootCartridge())

	obs := &cliObserver{json: jsonOutput}
	defer obs.close()
	host.SetObserver(obs)

	return host.Run(ctx)
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
	json       bool
	board      *board.Board
	tailCancel context.CancelFunc
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
		_ = startReportJSON(cfg, endpoint, bootErr)
		return
	}
	printRunningSummary(cfg, endpoint, bootErr)
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

// TeardownWarning surfaces a failed teardown step. Only the cartridge detach is
// worth a user-visible line: everything else is either logged or harmless.
func (o *cliObserver) TeardownWarning(step string, err error) {
	if step == vmhost.StepCartridge && !o.json {
		fmt.Printf("%s detach cartridge: %v\n", warning("⚠"), err)
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
func startReportJSON(cfg *config.Config, endpoint string, bootErr error) error {
	r := map[string]any{
		jsonFieldStatus: "running",
		"ssh_addr":      fmt.Sprintf("localhost:%d", cfg.LocalSSHPort),
		"api":           endpoint,
		"log":           cfg.LogPath,
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
		fmt.Println(warning("⚠ VM is running but the guest did not finish booting"))
		fmt.Printf("  %s %v\n", key("Reason:"), bootErr)
		fmt.Printf("  %s %s\n", key("Console:"), value(cfg.ConsoleLogPath))
		fmt.Printf("  %s %s\n", key("Hint:"), subtle("`br shell` and `br ssh` will fail until cloud-init completes."))
	}
	fmt.Printf("  %s %s\n", key("SSH:"), command("br ssh"))
	fmt.Printf("  %s %s\n", key("Shell:"), command("br shell"))
	fmt.Printf("  %s %s\n", key("API:"), value(endpoint))
	fmt.Println()
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
