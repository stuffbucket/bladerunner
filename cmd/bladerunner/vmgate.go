package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"github.com/stuffbucket/bladerunner/internal/cartridge"
	"github.com/stuffbucket/bladerunner/internal/control"
	"github.com/stuffbucket/bladerunner/internal/instance"
	"github.com/stuffbucket/bladerunner/internal/logging"
	"github.com/stuffbucket/bladerunner/internal/util"
	"github.com/stuffbucket/bladerunner/internal/vmhost"
)

// errVMNotRunning identifies the "the instance this verb needs is not running"
// condition. It is never returned as-is: notRunningError writes the message for
// the actual target and matches this under errors.Is, so a caller can test for
// the condition without matching on the wording. The raw control-socket dial
// failure is logged at debug level and never printed, so the terminal stays
// clean.
var errVMNotRunning = errors.New("the VM is not running")

// notRunning is that condition, carrying the message written for one target.
type notRunning struct{ message string }

// Error returns the advice-bearing message.
func (e *notRunning) Error() string { return e.message }

// Is makes every notRunning match errVMNotRunning under errors.Is, whatever its
// wording.
func (e *notRunning) Is(target error) bool {
	return target == errVMNotRunning
}

// instancesHint answers the question a "not running" message provokes: so what
// IS running? It is the last line of every one of them.
const instancesHint = "'br instances' lists what is running"

// notRunningError reports that the instance a verb targets is not running, and
// names the verb that brings THAT instance back.
//
// The message used to be "VM is not running; start it with 'br start'" for
// every target, and it is reached by web, exec, logs, events, incus, reconnect,
// ls and shell. For a disk slot or a cartridge that advice is not merely
// unhelpful, it is harmful: 'br start' creates an ADDITIONAL flat VM rather
// than bringing back the instance the user meant, so following it leaves two
// VMs where one was wanted and the original still down.
func notRunningError(target resolvedInstance) error {
	switch {
	case target.Fallback:
		// Nothing answered anywhere, so name both ways in: a flat VM and a
		// disk or cartridge.
		return &notRunning{message: "no VM is running\n" +
			"  start one with 'br up', or boot a disk or cartridge with 'br boot <name>'\n" +
			"  " + instancesHint}
	case target.isDefaultSlot():
		return &notRunning{message: "the default VM is not running\n" +
			"  start it with 'br up'\n" +
			"  " + instancesHint}
	default:
		return &notRunning{message: fmt.Sprintf(
			"instance %q (%s) is not running\n  boot it with 'br boot %s'\n  %s",
			target.instanceName(), target.Kind, bootArgument(target), instancesHint)}
	}
}

// bootArgument is what 'br boot' needs in order to bring target back: a
// cartridge boots from its image file, because the mounted volume is only a
// view of it; everything else boots by name.
func bootArgument(target resolvedInstance) string {
	if target.Kind == instance.KindCartridge && target.SourcePath != "" {
		return target.SourcePath
	}
	return target.instanceName()
}

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

// startVMDetachedAndWait brings the flat default instance up under a holder and
// waits until it answers.
//
// It used to fork `br start` detached and poll for its SSH config path — a
// SECOND way to run a VM out of process, alongside the holder the watcher
// spawns. There is now one: this spawns the same `br vmd` every other path
// does, and waits with the same attachment. `br start` itself would only have
// spawned a holder and exited, so the extra CLI in between bought nothing but a
// process that had to be kept alive long enough to hand over.
func startVMDetachedAndWait(stateDir string) error {
	spec := vmhost.Spec{
		Kind:          instance.KindFlat,
		StateDir:      stateDir,
		BinaryVersion: version,
	}
	fmt.Printf("%s Starting VM…\n", subtle("›"))
	if err := startUnderHolder(context.Background(), spec, holderAttachOptions{Quiet: true}); err != nil {
		return err
	}
	fmt.Println(success("✓ VM is running"))
	return nil
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

// holderSpawn describes a `br vmd` launch: the complete description of the one
// instance the holder is to own.
//
// It carries a whole vmhost.Spec rather than a handful of scalars because every
// ordinary path — `br start`, `br up`, `br boot` — now runs its VM under a
// holder, and those paths configure sizing, an image, a disk manifest, a
// restore file and the cartridge options. Mirroring each of them as a `br vmd`
// flag would grow the holder into a second copy of the `br start` flag set; a
// Spec is already declared to be serializable for exactly this hop.
type holderSpawn struct {
	// Spec is the instance to hold. Spec.StateDir is required: it is where the
	// holder's log and hand-off file go.
	Spec vmhost.Spec
}

// errHolderStateDir is returned when a holder is asked for without saying which
// instance it should hold.
var errHolderStateDir = errors.New("a holder needs a state directory")

// args builds the argv tail for `br vmd`. It is pure, so the exact command line
// a holder is launched with is testable without spawning anything.
//
// The Spec travels in a FILE rather than on the command line. A command line is
// world-readable through ps(1), it is bounded by ARG_MAX, and a JSON blob in an
// argv slot is unreadable in exactly the place — a wedged holder in a process
// listing — where a human most needs to read it.
func (h holderSpawn) args(specPath string) []string {
	return []string{vmdVerb, "--" + vmdSpecFlag, specPath}
}

// vmdVerb is the hidden subcommand a holder runs as.
const vmdVerb = "vmd"

// logName is the instance name this spawn's holder log is keyed on: the
// explicit name, else the cartridge's own name (a cartridge holder is spawned
// with the registry root as its state dir, so the name is the only thing that
// separates its log from every other cartridge's), else "" for the flat
// default.
func (h holderSpawn) logName() string {
	if h.Spec.Name != "" {
		return h.Spec.Name
	}
	if h.Spec.CartridgePath == "" {
		return ""
	}
	return cartridge.NameFromPath(h.Spec.CartridgePath)
}

// specPath is where this spawn's hand-off file goes: beside the holder log, and
// keyed the same way, so two instances spawned into one state directory (every
// cartridge shares the registry root) never overwrite each other's.
func (h holderSpawn) specPath() string {
	return filepath.Join(h.Spec.StateDir, vmdSpecName(h.logName()))
}

// specPerm is the mode of the hand-off file. It names the user's image paths
// and state directory and is nobody else's business.
const specPerm = 0o600

// stateDirPerm is the mode a state directory is created with when a spawn has
// to materialize one (a disk slot booted for the first time). It matches what
// internal/config uses for the state dir it creates.
const stateDirPerm = 0o755

// writeHolderSpec serializes the Spec for the holder to pick up and returns the
// path it was written to.
func writeHolderSpec(spawn holderSpawn) (string, error) {
	blob, err := json.Marshal(spawn.Spec)
	if err != nil {
		return "", fmt.Errorf("encode holder spec: %w", err)
	}
	path := spawn.specPath()
	if err := util.WriteFileAtomic(path, blob, specPerm); err != nil {
		return "", fmt.Errorf("write holder spec: %w", err)
	}
	return path, nil
}

// readHolderSpec loads a hand-off file and consumes it.
//
// The file is removed only after it parsed: a spec that could not be read is
// the one a human will want to look at, and leaving it costs a few hundred
// bytes in the state directory.
func readHolderSpec(path string) (vmhost.Spec, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return vmhost.Spec{}, fmt.Errorf("read holder spec: %w", err)
	}
	var spec vmhost.Spec
	if err := json.Unmarshal(blob, &spec); err != nil {
		return vmhost.Spec{}, fmt.Errorf("decode holder spec %s: %w", path, err)
	}
	if err := os.Remove(path); err != nil {
		logging.L().Warn("could not remove the holder hand-off file", "path", path, "err", err)
	}
	return spec, nil
}

// spawnHolder starts a detached `br vmd` for one instance and returns its PID.
//
// The holder is a re-exec of this same signed binary (see vmd.go for why), it
// runs in its own session so nothing that happens to this process reaches it,
// and its output goes to the instance's own holder log because it has no
// terminal to write to. This function returns as soon as the child is running;
// readiness is observed through the control socket and the instance registry
// the holder publishes, not by waiting on the process.
//
// It is the ONE way a VM is started out-of-process. `br start`, `br up`,
// `br boot`, the auto-start behind verbs that need a VM, `br watch` and the
// menubar watcher all reach the VM through here.
func spawnHolder(spawn holderSpawn) (int, error) {
	if spawn.Spec.StateDir == "" {
		return 0, errHolderStateDir
	}
	// A disk slot booted for the first time has no directory yet, and both the
	// log and the hand-off file live in it. config.Default does not create it
	// either; the Host would, far too late to log this spawn.
	if err := os.MkdirAll(spawn.Spec.StateDir, stateDirPerm); err != nil {
		return 0, fmt.Errorf("create state directory: %w", err)
	}
	specPath, err := writeHolderSpec(spawn)
	if err != nil {
		return 0, err
	}
	logFile, err := openVMDLog(spawn.Spec.StateDir, spawn.logName())
	if err != nil {
		_ = os.Remove(specPath)
		return 0, err
	}
	defer func() { _ = logFile.Close() }()

	pid, err := spawnDetached(detachedSpawn{Args: spawn.args(specPath), Stdio: logFile})
	if err != nil {
		_ = os.Remove(specPath)
		return 0, fmt.Errorf("start holder: %w", err)
	}
	return pid, nil
}
