package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/boot"
	"github.com/stuffbucket/bladerunner/internal/bootstage"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/control"
	"github.com/stuffbucket/bladerunner/internal/logging"
	"github.com/stuffbucket/bladerunner/internal/oidc"
	"github.com/stuffbucket/bladerunner/internal/ssh"
	"github.com/stuffbucket/bladerunner/internal/timesource"
	"github.com/stuffbucket/bladerunner/internal/ui"
	"github.com/stuffbucket/bladerunner/internal/ui/board"
	"github.com/stuffbucket/bladerunner/internal/vm"
	"github.com/stuffbucket/bladerunner/internal/webproxy"
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
	cloudInit   bool
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
	f.BoolVar(&startFlags.cloudInit, "cloud-init", false, "Escape hatch: force the legacy Debian genericcloud + first-boot cloud-init path instead of the default pre-baked guest image (also settable via BLADERUNNER_FORCE_CLOUD_INIT=1)")
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

// registerUpgradeHandlers registers the control commands that back `runner
// upgrade` and `br eject`: reporting the server's build version,
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

// applyFlagOverrides applies the `start` CLI flags onto cfg, on top of the
// already-overlaid Settings and disk manifest. When driven is true (a
// boot/cartridge boot stuffed pre-resolved precedence into startFlags) every
// flag is applied verbatim; otherwise only flags the user explicitly changed
// (per the changed predicate, normally cmd.Flags().Changed) are applied, so the
// persisted Settings baseline is not clobbered by cobra's flag defaults.
func applyFlagOverrides(cfg *config.Config, changed func(string) bool, driven bool) {
	apply := func(name string) bool { return driven || changed(name) }

	if apply("cpus") {
		cfg.CPUs = startFlags.cpus
	}
	if apply("memory") {
		cfg.MemoryGiB = startFlags.memory
	}
	if apply("disk") {
		cfg.DiskSizeGiB = startFlags.disk
	}
	if apply("gui") {
		cfg.GUI = startFlags.gui
	}
	if apply("timeout") {
		cfg.WaitForIncus = startFlags.timeout
	}
	// UseGuestAgent is derived from two flags; recompute if either was set.
	if apply("use-guest-agent") || apply("no-agent") {
		cfg.UseGuestAgent = startFlags.useAgent && !startFlags.noAgent
	}
	// --cloud-init is the escape hatch off the #155 default: force the legacy
	// pinned-Debian + first-boot cloud-init path. It resolves the Debian URL/SHA
	// and disables the hosted image + agent handshake (and its auto-fallback), so
	// the whole boot follows the cloud-init path. A custom --image-url/--image-path
	// below still wins (it sets an explicit source), but the agent stays off.
	if apply("cloud-init") && startFlags.cloudInit {
		if url, err := config.DebianTrixieGenericCloudURL(cfg.Arch); err == nil {
			cfg.BaseImageURL = url
			cfg.BaseImageSHA512 = config.DebianTrixieGenericCloudSHA512(cfg.Arch)
		}
		cfg.UseHostedGuestImage = false
		cfg.UseGuestAgent = false
		cfg.HostedImageFallback = false
	}
	if apply("no-nested-virt") {
		cfg.NestedVirtDisabled = startFlags.noNested
	}
	// Image flags keep their "non-empty means set" guard: a boot/cartridge start
	// clears them (it carries the image via the manifest), and a plain start
	// leaves them empty unless the user passed one.
	if startFlags.imageURL != "" && apply("image-url") {
		cfg.BaseImageURL = startFlags.imageURL
		// A custom image isn't the pinned Debian default nor the pre-baked hosted
		// default, so the embedded SHA-512 no longer applies (fall back to sidecar
		// verification) and neither the hosted flag nor its auto-fallback apply.
		cfg.BaseImageSHA512 = ""
		cfg.UseHostedGuestImage = false
		cfg.HostedImageFallback = false
	}
	if startFlags.imagePath != "" && apply("image-path") {
		cfg.BaseImagePath = startFlags.imagePath
		// A local image bypasses download/verify entirely; clear the hosted
		// auto-fallback so ensureBaseImage takes the BaseImagePath branch.
		cfg.UseHostedGuestImage = false
		cfg.HostedImageFallback = false
	}
}

//nolint:gocyclo // runStart was already at the gocyclo ceiling; the applyBootManifest guard for `br boot` tips it one over with essential error propagation.
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
		return fmt.Errorf("VM is already running (use 'br stop' first)")
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

	// Overlay the persisted, host-wide user settings (defaults -> Settings)
	// BEFORE the manifest and flags so a user's saved preferences become the
	// baseline. Settings live under the default state dir (not a custom
	// --state-dir slot or a cartridge), matching the menubar's settings screen.
	// A missing file yields defaults (no-op); an invalid file is logged once
	// logging is up and ignored in favor of defaults rather than aborting start.
	settings, settingsErr := config.LoadSettings(config.DefaultStateDir())
	if settingsErr != nil {
		settings = config.DefaultSettings()
	}
	settings.ApplyTo(cfg)

	// Apply a disk manifest (set by `br boot`) as defaults AFTER Settings but
	// BEFORE the flag overrides below, so the manifest's image/sizing/boot-mode
	// overrides saved Settings and explicit flags still win. No-op for a plain
	// `br start`.
	if err := applyBootManifest(cfg); err != nil {
		return err
	}

	// Apply CLI flags. On a boot/cartridge-driven start the flags carry
	// pre-resolved precedence (flag-or-manifest-or-default, incl. a --headless
	// override of a GUI manifest) and are applied verbatim; on a plain `br
	// start` only flags the user explicitly changed are applied, so the
	// persisted Settings overlaid above are not clobbered by flag defaults.
	driven := bootManifest != nil || bootCartridge.mountpoint != ""
	applyFlagOverrides(cfg, cmd.Flags().Changed, driven)

	// A cartridge boot roots every per-VM path inside the mounted image and wires
	// the RW share. This must land AFTER the manifest/flag overrides so the
	// cartridge's own root.img / state / share win. No-op for a non-cartridge boot.
	applyBootCartridge(cfg)

	// Setup logging
	if err := logging.Init(cfg.LogPath); err != nil {
		return err
	}
	if settingsErr != nil {
		logging.L().Warn("ignoring invalid settings; using defaults", "err", settingsErr)
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
	if cfg.LocalNTPPort != 0 && cfg.VsockNTPPort != 0 {
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
	// instead of cold-booting. Used by `br restore` and `br upgrade`.
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

	// Publish coarse, human-friendly boot phase to the bootstage file for the
	// menubar's starting splash. Driven by the runner's own stage events — which
	// fire in every mode (GUI, headless, detached menubar boot) and don't depend
	// on racing/parsing the serial console — so a separate process that only sees
	// the control socket can still show "Booting Linux… / Setting up… / Starting
	// Incus…" as the guest comes up. Cleared on the way out.
	bootPub := newBootStagePublisher(cfg.VMDir)
	defer bootstage.Clear(cfg.VMDir)

	// Build the buildx-style boot board when stderr is a TTY. It shows
	// stage state on top and a live tail of the guest serial console
	// underneath. Non-TTY callers (CI, log capture) still get plain slog
	// output via the noop board path. In --json mode we skip it entirely so
	// the only stdout output is the final JSON report.
	var brd *board.Board
	var boardProg vm.Progress
	tailCancel := context.CancelFunc(func() {})
	if !jsonOutput {
		brd, boardProg, tailCancel = startBootBoard(ctx, cfg)
	}
	defer tailCancel()

	// Attach progress sinks: always the bootstage file publisher, plus the TTY
	// board when present.
	reporters := []vm.Progress{&bootStageProgress{pub: bootPub}}
	if boardProg != nil {
		reporters = append(reporters, boardProg)
	}
	runner.SetProgress(teeProgress(reporters))

	result, err := runner.StartVM(ctx)
	if err != nil {
		if brd != nil {
			brd.Stop()
		}
		return fmt.Errorf("start vm: %w", err)
	}

	// Now that the VM (and its vsock device) exists, teach `br status` to probe
	// guest liveness instead of trusting the host run-state alone. A panicked
	// or unreachable guest now reports "unreachable" rather than "running".
	ctrl.SetProbe(func(ctx context.Context) error {
		pctx, cancelProbe := context.WithTimeout(ctx, 2*time.Second)
		defer cancelProbe()
		return runner.ProbeGuest(pctx)
	})

	// Publish the resolved nested-virt state so `br status` can report whether
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

	// Start the host-side web-UI proxy. It terminates the browser's TLS WITHOUT
	// requesting a client certificate (so the browser never shows the cert
	// picker), forwarding to Incus over loopback with no client cert of its own
	// — so Incus authenticates the browser via OIDC. `br web` points the browser
	// here (LocalWebPort) instead of straight at Incus. Non-fatal: a failure just
	// means `br web` falls back to the direct Incus URL (with the cert prompt).
	if webProxy, werr := webproxy.New(webproxy.Options{
		ListenAddr:   fmt.Sprintf("127.0.0.1:%d", cfg.LocalWebPort),
		UpstreamAddr: fmt.Sprintf("127.0.0.1:%d", cfg.LocalAPIPort),
		CertPath:     filepath.Join(cfg.VMDir, "webproxy.crt"),
		KeyPath:      filepath.Join(cfg.VMDir, "webproxy.key"),
	}); werr != nil {
		logging.L().Warn("web proxy not created", "err", werr)
	} else if werr := webProxy.Start(); werr != nil {
		logging.L().Warn("web proxy not started", "err", werr)
	} else {
		defer func() { _ = webProxy.Close() }()
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
// file publisher plus the optional TTY board).
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
