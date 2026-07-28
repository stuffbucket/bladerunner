package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/stuffbucket/bladerunner/internal/cartridge"
	"github.com/stuffbucket/bladerunner/internal/control"
	"github.com/stuffbucket/bladerunner/internal/logging"
)

// errVMNotRunning is returned, with a clean message that omits the raw
// control-socket dial failure, when a command needs the VM but it is not running
// and was not started. The underlying socket error is logged at debug level.
var errVMNotRunning = errors.New("VM is not running; start it with 'br start'")

// notRunningError reports that the instance a verb targets is not running.
func notRunningError(target resolvedInstance) error {
	if target.isDefaultSlot() {
		return errVMNotRunning
	}
	return fmt.Errorf("instance %q is not running", target.instanceName())
}

// vmStartReadyTimeout bounds how long requireRunningVM waits for an auto-started
// VM to publish its readiness signal.
const vmStartReadyTimeout = 3 * time.Minute

// requireRunningVM returns a control client for the instance a verb resolved.
//
// When that instance is not running and it is the flat default, it offers to
// start it on an interactive terminal; otherwise (or if the user declines) it
// returns notRunningError. Every other instance — a disk slot, a cartridge — is
// never auto-started, because bringing one back needs its own source and
// 'br boot' owns that. The raw control-socket dial failure is logged, never
// printed, so the terminal stays clean.
//
// Commands that need a VM funnel through requireRunningTarget, which resolves
// --instance and then calls this, rather than touching the control client
// directly.
func requireRunningVM(target resolvedInstance) (*control.Client, error) {
	client := control.NewClient(target.StateDir)
	if client.IsRunning() {
		return client, nil
	}
	// Log the detail for `BLADERUNNER_LOG_LEVEL=debug`; keep it off the terminal.
	logging.L().Debug("VM control socket unreachable; VM not running",
		"instance", target.instanceName(), "socket", control.SocketPath(target.StateDir))

	if !target.isDefaultSlot() || !interactiveTerminal() || !confirmStartVM() {
		return nil, notRunningError(target)
	}
	if err := startVMDetachedAndWait(target.StateDir); err != nil {
		return nil, err
	}
	return client, nil
}

// interactiveTerminal reports whether both stdin and stdout are TTYs, so a
// [Y/n] prompt can be shown and answered.
func interactiveTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// confirmStartVM shows a [Y/n] prompt (default yes) asking to start the VM.
func confirmStartVM() bool {
	fmt.Printf("%s The VM is not running. Start it now? %s ", subtle("›"), subtle("[Y/n]"))
	return confirmStartVMFrom(os.Stdin)
}

// confirmStartVMFrom parses a [Y/n] answer (default yes on empty/EOF) from r.
func confirmStartVMFrom(r io.Reader) bool {
	line, err := bufio.NewReader(r).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		// Treat empty input as the default "yes" only when the line ended
		// cleanly (newline) or at EOF after no characters; a read error with a
		// partial line is declined below.
		if err != nil && line == "" {
			// EOF with no input → accept the default (yes).
			return errors.Is(err, io.EOF)
		}
		return true
	default:
		return false
	}
}

// startVMDetachedAndWait launches `br start` as a detached background process
// (so it outlives this short-lived command and becomes the VM host) and waits
// until the VM publishes its SSH config path — the signal that StartVM has
// returned and the VM is up — or the timeout elapses.
func startVMDetachedAndWait(stateDir string) error {
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", os.DevNull, err)
	}
	defer func() { _ = devnull.Close() }()

	pid, err := spawnDetached(detachedSpawn{Args: []string{"start"}, Stdio: devnull})
	if err != nil {
		return fmt.Errorf("start VM: %w", err)
	}

	fmt.Printf("%s Starting VM (pid %d)…\n", subtle("›"), pid)

	client := control.NewClient(stateDir)
	deadline := time.Now().Add(vmStartReadyTimeout)
	for time.Now().Before(deadline) {
		if client.IsRunning() {
			if v, _ := client.GetConfig(control.ConfigKeySSHConfigPath); v != "" {
				fmt.Println(success("✓ VM is running"))
				return nil
			}
		}
		time.Sleep(750 * time.Millisecond)
	}
	return errors.New("timed out waiting for the VM to start; check 'br status' and the log")
}

// detachedSpawn describes a re-exec of this very binary as a process that
// outlives the caller.
type detachedSpawn struct {
	// Args is the argv tail handed to the re-executed `br`.
	Args []string
	// Stdio receives the child's stdin, stdout and stderr. It must be non-nil:
	// a detached child inherits no terminal, and leaving stdio attached to the
	// parent's would keep the parent's pipes open and block whatever is reading
	// them.
	Stdio *os.File
}

// spawnDetached re-execs this binary and returns the child's PID.
//
// Three things make the child survive the parent, and all three are required:
//
//   - setsid (detachProcess) puts it in a new session with no controlling
//     terminal, so closing the terminal — which sends SIGHUP to the foreground
//     process group — never reaches it;
//   - stdio is redirected away from the parent's descriptors, so the child does
//     not die on SIGPIPE and does not hold the parent's pipes open;
//   - Process.Release drops the parent's handle, so nothing ever waits on it
//     and the child is reparented to init when the parent exits.
func spawnDetached(spawn detachedSpawn) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("locate br executable: %w", err)
	}
	if spawn.Stdio == nil {
		return 0, errors.New("detached spawn needs somewhere to send its output")
	}

	// context.Background(): the child must outlive this short-lived command, so
	// it is intentionally not bound to a cancelable context.
	cmd := exec.CommandContext(context.Background(), exe, spawn.Args...)
	cmd.Stdin = spawn.Stdio
	cmd.Stdout = spawn.Stdio
	cmd.Stderr = spawn.Stdio
	detachProcess(cmd) // platform-specific: run in a new session
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	// The started process is the long-lived VM host; do not wait on it.
	_ = cmd.Process.Release()
	return pid, nil
}

// holderSpawn describes a `br vmd` launch: which instance to hold, and how.
type holderSpawn struct {
	// StateDir is the instance's state directory. Required — it is both the
	// holder's only mandatory argument and where its log file goes.
	StateDir string
	// CartridgePath, when set, makes the holder attach and boot a cartridge.
	CartridgePath string
	// Name overrides the instance name derived from the state directory.
	Name string
	// GUI opens the VM console window.
	GUI bool
	// DrainTimeout bounds the orderly guest shutdown. Zero uses the default.
	DrainTimeout time.Duration
}

// errHolderStateDir is returned when a holder is asked for without saying which
// instance it should hold.
var errHolderStateDir = errors.New("a holder needs a state directory")

// args builds the argv tail for `br vmd`. It is pure, so the exact command line
// a holder is launched with is testable without spawning anything.
func (h holderSpawn) args() []string {
	args := []string{"vmd", "--state-dir", h.StateDir}
	if h.CartridgePath != "" {
		args = append(args, "--cartridge", h.CartridgePath)
	}
	if h.Name != "" {
		args = append(args, "--name", h.Name)
	}
	if h.GUI {
		args = append(args, "--gui")
	}
	if h.DrainTimeout > 0 {
		args = append(args, "--drain-timeout", h.DrainTimeout.String())
	}
	return args
}

// logName is the instance name this spawn's holder log is keyed on: the
// explicit name, else the cartridge's own name (a cartridge holder is spawned
// with the registry root as its state dir, so the name is the only thing that
// separates its log from every other cartridge's), else "" for the flat
// default.
func (h holderSpawn) logName() string {
	if h.Name != "" {
		return h.Name
	}
	if h.CartridgePath == "" {
		return ""
	}
	return cartridge.NameFromPath(h.CartridgePath)
}

// spawnHolder starts a detached `br vmd` for one instance and returns its PID.
//
// The holder is a re-exec of this same signed binary (see vmd.go for why), it
// runs in its own session so nothing that happens to this process reaches it,
// and its output goes to the instance's own holder log because it has no
// terminal to write to. This function returns as soon as the child is running;
// readiness is observed through the control socket and the instance registry
// the holder publishes, not by waiting on the process.
func spawnHolder(spawn holderSpawn) (int, error) {
	if spawn.StateDir == "" {
		return 0, errHolderStateDir
	}
	logFile, err := openVMDLog(spawn.StateDir, spawn.logName())
	if err != nil {
		return 0, err
	}
	defer func() { _ = logFile.Close() }()

	pid, err := spawnDetached(detachedSpawn{Args: spawn.args(), Stdio: logFile})
	if err != nil {
		return 0, fmt.Errorf("start holder: %w", err)
	}
	return pid, nil
}
