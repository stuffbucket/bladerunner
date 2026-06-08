package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/boot"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/control"
	"github.com/stuffbucket/bladerunner/internal/logging"
	"github.com/stuffbucket/bladerunner/internal/oidc"
	"github.com/stuffbucket/bladerunner/internal/ssh"
	"github.com/stuffbucket/bladerunner/internal/timesource"
	"github.com/stuffbucket/bladerunner/internal/ui"
	"github.com/stuffbucket/bladerunner/internal/ui/board"
	"github.com/stuffbucket/bladerunner/internal/vm"
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
	timeout     time.Duration
	useAgent    bool
	noAgent     bool
	noNested    bool
	restoreFrom string
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
	f.DurationVar(&startFlags.timeout, "timeout", config.DefaultTimeout, "Wait timeout for Incus")
	f.BoolVar(&startFlags.useAgent, "use-guest-agent", false, "Use the in-guest br-agent for boot config (requires pre-baked image or user-side install)")
	f.BoolVar(&startFlags.noAgent, "no-agent", false, "Force the legacy cloud-init/HTTP-polling path even if use-guest-agent is enabled")
	f.BoolVar(&startFlags.noNested, "no-nested-virt", false, "Disable nested virtualization even if the host supports it (Incus VMs will be unavailable)")
	f.StringVar(&startFlags.restoreFrom, "restore", "", "Restore the guest from a saved-state file (see 'runner save') instead of cold-booting")
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

// registerUpgradeHandlers registers the control commands that back `runner
// upgrade` and `runner eject`: reporting the server's build version,
// pausing+saving the guest state, and the clean ACPI shutdown. getRunner returns
// the active runner once StartVM has created it (nil before). cancel unblocks the
// foreground runStart so the process exits after a graceful eject — the deferred
// runner.Stop() then tears the VMM down and the deferred cartridge detach (if
// any) runs last.
func registerUpgradeHandlers(router *control.Router, cfg *config.Config, getRunner func() *vm.Runner, cancel context.CancelFunc) {
	router.HandleFunc(control.CmdServerVersion, func(_ context.Context, _ *control.Request) *control.Message {
		return &control.Message{Response: version}
	})
	router.HandleFunc(control.CmdSave, func(_ context.Context, req *control.Request) *control.Message {
		r := getRunner()
		if r == nil {
			return &control.Message{Error: "VM is not started yet"}
		}
		if err := r.SaveState(cfg.SavedStatePath); err != nil {
			return &control.Message{Error: err.Error()}
		}
		if req.Args["0"] != control.SaveModePause {
			if err := r.ResumeVM(); err != nil {
				return &control.Message{Error: err.Error()}
			}
		}
		return &control.Message{Response: cfg.SavedStatePath}
	})
	router.HandleFunc(control.CmdEject, func(ctx context.Context, req *control.Request) *control.Message {
		r := getRunner()
		if r == nil {
			return &control.Message{Error: "VM is not started yet"}
		}
		timeout := ejectTimeoutFromArgs(req)
		force := ejectForceFromArgs(req)
		// Gracefully (ACPI) shut the guest down and wait for it to stop. Detach of
		// any cartridge is NOT done here: the VMM still holds root.img until the
		// deferred runner.Stop() runs after the foreground unblocks. We only stop
		// the guest, then cancel to unblock runStart so its deferred Stop() +
		// cartridge detach run in the right order.
		if err := r.Eject(ctx, timeout, force); err != nil {
			return &control.Message{Error: err.Error()}
		}
		cancel()
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

//nolint:gocyclo // runStart was already at the gocyclo ceiling; the applyBootManifest guard for `runner boot` tips it one over with essential error propagation.
func runStart(cmd *cobra.Command, args []string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// A cartridge boot OWNS the mounted image: detach it on the way out. This
	// defer is registered first so, running LIFO, it executes LAST — after the
	// deferred runner.Stop() below tears the VMM down and releases root.img.
	defer detachBootCartridge()

	// Build config
	cfg, err := config.Default(startFlags.stateDir)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	// Check if already running
	client := control.NewClient(cfg.VMDir)
	if client.IsRunning() {
		return fmt.Errorf("VM is already running (use 'runner stop' first)")
	}

	// Start control server. We build the controller explicitly (rather than
	// via NewServer) so a guest-liveness probe can be attached once the VM is
	// running — see runner.ProbeGuest below.
	ctrl := control.NewLocalController(cancel)
	ctrlServer, err := control.NewListener(cfg.VMDir, ctrl)
	if err != nil {
		return fmt.Errorf("start control server: %w", err)
	}
	defer func() { _ = ctrlServer.Close() }()

	// Mount config handler (captures cfg by reference; sees values set after VM start)
	cfgHandler := control.NewConfigRouter(cfg)
	ctrlServer.Router().Mount("config", cfgHandler.Router())

	// Synchronized holder so the save handler (registered before the server
	// starts serving, to avoid a handlers-map race) can reach the runner once
	// it exists.
	var (
		runnerMu     sync.Mutex
		activeRunner *vm.Runner
	)
	setRunner := func(r *vm.Runner) { runnerMu.Lock(); activeRunner = r; runnerMu.Unlock() }
	registerUpgradeHandlers(ctrlServer.Router(), cfg, func() *vm.Runner {
		runnerMu.Lock()
		defer runnerMu.Unlock()
		return activeRunner
	}, cancel)

	go ctrlServer.Start(ctx)

	// Apply a disk manifest (set by `runner boot`) as defaults BEFORE the flag
	// overrides below, so the manifest's image/sizing/boot-mode lands first and
	// explicit flags still win. No-op for a plain `runner start`.
	if err := applyBootManifest(cfg); err != nil {
		return err
	}

	// Apply flags
	cfg.CPUs = startFlags.cpus
	cfg.MemoryGiB = startFlags.memory
	cfg.DiskSizeGiB = startFlags.disk
	cfg.GUI = startFlags.gui
	cfg.WaitForIncus = startFlags.timeout
	cfg.UseGuestAgent = startFlags.useAgent && !startFlags.noAgent
	cfg.NestedVirtDisabled = startFlags.noNested

	if startFlags.imageURL != "" {
		cfg.BaseImageURL = startFlags.imageURL
		// A custom image isn't the pinned Debian default, so the embedded
		// SHA-512 no longer applies; fall back to sidecar verification.
		cfg.BaseImageSHA512 = ""
	}
	if startFlags.imagePath != "" {
		cfg.BaseImagePath = startFlags.imagePath
	}

	// A cartridge boot roots every per-VM path inside the mounted image and wires
	// the RW share. This must land AFTER the manifest/flag overrides so the
	// cartridge's own root.img / state / share win. No-op for a non-cartridge boot.
	applyBootCartridge(cfg)

	// Setup logging
	if err := logging.Init(cfg.LogPath); err != nil {
		return err
	}

	// Ensure SSH keys
	keyPair, err := ssh.EnsureKeyPair()
	if err != nil {
		return fmt.Errorf("ssh keys: %w", err)
	}
	cfgHandler.Lock()
	cfg.SetSSHKeys(keyPair.PublicKey, keyPair.PrivateKeyPath)
	cfgHandler.Unlock()

	// Start the local OIDC provider before the VM so the vsock-reverse forwarder
	// can dial it as soon as the guest comes up. Failure to start OIDC is logged
	// but does not abort start; the mTLS fallback path remains available.
	oidcProvider, err := startOIDCProvider(ctx, cfg, keyPair.PublicKey)
	switch {
	case errors.Is(err, errOIDCDisabled):
		logging.L().Info("oidc provider disabled by config")
	case err != nil:
		logging.L().Warn("oidc provider not started", "err", err)
	default:
		defer func() { _ = oidcProvider.Stop() }()
	}

	// Start the host pseudo-NTP (SNTP) responder before the VM so the vsock
	// reverse forwarder can dial it the moment the guest chrony polls. The
	// responder serves the HOST clock as a stratum-1 source; the guest coheres to
	// the host (not UTC) and works offline over vsock. Non-fatal: chrony retries.
	if cfg.LocalNTPPort != 0 {
		ntpResponder, nerr := timesource.NewResponder(fmt.Sprintf("127.0.0.1:%d", cfg.LocalNTPPort))
		if nerr != nil {
			logging.L().Warn("ntp responder not started", "err", nerr)
		} else {
			ntpResponder.Start()
			defer func() { _ = ntpResponder.Stop() }()
		}
	}

	// Create and start VM
	runner, err := vm.NewRunner(cfg)
	if err != nil {
		return fmt.Errorf("create runner: %w", err)
	}
	defer func() { _ = runner.Stop() }()
	setRunner(runner)

	// --restore: bring the guest up from a saved-state file (and resume it)
	// instead of cold-booting. Used by `runner restore` and `runner upgrade`.
	if startFlags.restoreFrom != "" {
		runner.SetRestoreFrom(startFlags.restoreFrom)
	}

	if !jsonOutput {
		fmt.Println(title("Starting Bladerunner VM..."))
		fmt.Printf("  %s %s\n", key("Name:"), value(cfg.Name))
		fmt.Printf("  %s %d\n", key("CPUs:"), cfg.CPUs)
		fmt.Printf("  %s %d GiB\n", key("Memory:"), cfg.MemoryGiB)
		fmt.Printf("  %s %s\n", key("Arch:"), value(runtime.GOARCH))
		fmt.Printf("  %s %s\n", key("Incus VMs:"), nestedVirtBanner())
		fmt.Println()
	}

	// Build the buildx-style boot board when stderr is a TTY. It shows
	// stage state on top and a live tail of the guest serial console
	// underneath. Non-TTY callers (CI, log capture) still get plain slog
	// output via the noop board path. In --json mode we skip it entirely so
	// the only stdout output is the final JSON report.
	var brd *board.Board
	tailCancel := context.CancelFunc(func() {})
	if !jsonOutput {
		brd, tailCancel = startBootBoard(ctx, cfg, runner)
	}
	defer tailCancel()

	result, err := runner.StartVM(ctx)
	if err != nil {
		if brd != nil {
			brd.Stop()
		}
		return fmt.Errorf("start vm: %w", err)
	}

	// Now that the VM (and its vsock device) exists, teach `runner status` to probe
	// guest liveness instead of trusting the host run-state alone. A panicked
	// or unreachable guest now reports "unreachable" rather than "running".
	ctrl.SetProbe(func(ctx context.Context) error {
		pctx, cancelProbe := context.WithTimeout(ctx, 2*time.Second)
		defer cancelProbe()
		return runner.ProbeGuest(pctx)
	})

	// Publish the resolved nested-virt state so `runner status` can report whether
	// Incus VMs are available in this guest.
	cfgHandler.Lock()
	cfg.NestedVirt = runner.NestedVirtState()
	cfgHandler.Unlock()

	// Write SSH config after VM starts
	sshConfigPath, err := ssh.WriteSSHConfig(cfg.LocalSSHPort, cfg.SSHUser, cfg.SSHPrivateKeyPath)
	if err != nil {
		logging.L().Warn("ssh config", "error", err)
	} else {
		cfgHandler.Lock()
		cfg.SSHConfigPath = sshConfigPath
		cfgHandler.Unlock()
	}

	// In headless mode we block the foreground on Incus readiness so the
	// board can render the full boot through to "ready" before yielding to
	// the SIGINT wait. In GUI mode we tear the board down first because
	// StartGUI takes over the macOS event loop and the user is watching
	// the guest window, not the terminal.
	// report emits the running summary as JSON or human text, depending on the
	// --json flag, after tearing down the boot board.
	report := func(bootErr error) {
		if brd != nil {
			brd.Stop()
			tailCancel()
		}
		if jsonOutput {
			_ = startReportJSON(cfg, result.Endpoint, bootErr)
			return
		}
		printRunningSummary(cfg, result.Endpoint, bootErr)
	}

	if cfg.GUI {
		// GUI mode can't block on Incus before opening the window — the
		// macOS event loop must run on the main thread immediately. We
		// don't yet know if boot will succeed, so don't claim it did.
		report(nil)
		go func() { _ = waitForGuestReady(ctx, cfg, runner) }()

		if !jsonOutput {
			fmt.Println(subtle("Opening GUI window (runs on main thread)..."))
		}
		if err := runner.StartGUI(); err != nil {
			return fmt.Errorf("start gui: %w", err)
		}
	} else {
		bootErr := waitForGuestReady(ctx, cfg, runner)
		report(bootErr)
		if !jsonOutput {
			fmt.Println(subtle("Headless mode. Press Ctrl+C to stop."))
		}
		<-ctx.Done()
	}

	if !jsonOutput {
		fmt.Println(subtle("\nShutting down..."))
	}
	return nil
}

// startReportJSON emits a one-line JSON object describing the running VM, used
// by `runner start --json`. The process keeps running afterward (start is a
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

// waitForGuestReady runs the agent handshake (when enabled) and the Incus
// readiness wait. Returns nil if the guest reached the Incus-ready state,
// or an error describing why it didn't. Errors are non-fatal at the call
// site (partial reports are still useful) but the caller should warn the
// user rather than pretend everything is fine.
func waitForGuestReady(ctx context.Context, cfg *config.Config, runner *vm.Runner) error {
	if cfg.UseGuestAgent {
		if _, err := runner.RunAgentHandshake(ctx); err != nil {
			logging.L().Warn("agent handshake failed, falling back to http wait", "error", err)
		}
	}
	if _, err := runner.WaitForIncus(ctx); err != nil {
		logging.L().Error("wait for incus", "error", err)
		return err
	}
	return nil
}

func printRunningSummary(cfg *config.Config, endpoint string, bootErr error) {
	fmt.Println()
	if bootErr == nil {
		fmt.Println(success("✓ VM is running"))
	} else {
		fmt.Println(warning("⚠ VM is running but the guest did not finish booting"))
		fmt.Printf("  %s %v\n", key("Reason:"), bootErr)
		fmt.Printf("  %s %s\n", key("Console:"), value(cfg.ConsoleLogPath))
		fmt.Printf("  %s %s\n", key("Hint:"), subtle("`runner shell` and `runner ssh` will fail until cloud-init completes."))
	}
	fmt.Printf("  %s %s\n", key("SSH:"), command("runner ssh"))
	fmt.Printf("  %s %s\n", key("Shell:"), command("runner shell"))
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
func startBootBoard(ctx context.Context, cfg *config.Config, runner *vm.Runner) (*board.Board, context.CancelFunc) {
	if !term.IsTerminal(int(os.Stderr.Fd())) {
		return nil, func() {}
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
	runner.SetProgress(newBoardAdapter(brd))

	tailCtx, cancel := context.WithCancel(ctx)
	go tailConsoleIntoBoard(tailCtx, brd, cfg.ConsoleLogPath)
	return brd, cancel
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

// errOIDCDisabled signals that the OIDC provider was intentionally skipped
// (e.g. LocalOIDCPort=0). Callers should treat it as a benign no-op.
var errOIDCDisabled = fmt.Errorf("oidc disabled (LocalOIDCPort=0)")

// startOIDCProvider boots the local OIDC server, registers the host's own SSH
// public key as the bootstrap admin identity, and returns the running provider.
// Returns errOIDCDisabled (with a nil provider) when OIDC is disabled by config;
// other errors mean startup failed and the caller should log and continue.
func startOIDCProvider(ctx context.Context, cfg *config.Config, hostPublicKey string) (*oidc.Provider, error) {
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
	if hostPublicKey != "" && store.Count() == 0 {
		if _, err := store.Add(hostPublicKey); err != nil {
			logging.L().Warn("auto-import host key failed", "err", err)
		}
	}

	provider, err := oidc.NewProvider(oidc.Config{
		ListenAddr: fmt.Sprintf("127.0.0.1:%d", cfg.LocalOIDCPort),
		IssuerURL:  cfg.OIDCIssuerURL,
		Audience:   cfg.OIDCAudience,
		SigningKey: signingKey,
		Store:      store,
	})
	if err != nil {
		return nil, err
	}
	if err := provider.Start(ctx); err != nil {
		return nil, err
	}
	return provider, nil
}
