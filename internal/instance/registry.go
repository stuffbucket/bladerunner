package instance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/stuffbucket/bladerunner/internal/logging"
	"github.com/stuffbucket/bladerunner/internal/util"
)

const (
	// dirName is the registry directory under the state dir.
	dirName = "instances"
	// entryExt is the extension of a registry record.
	entryExt = ".json"

	// dirPerm restricts the registry to its owner: entries expose socket paths
	// and working-copy locations for live VMs.
	dirPerm fs.FileMode = 0o700
	// entryPerm matches dirPerm's intent for the records themselves.
	entryPerm fs.FileMode = 0o600

	// controlSocketName mirrors internal/control.SocketName. It is duplicated
	// rather than imported because the control package is a consumer of this
	// registry; importing it here would create a cycle. Keep the two in sync.
	controlSocketName = "control.sock"

	// probeTimeout bounds the control-socket dial in DefaultProbe. A unix
	// socket connect is either accepted by the kernel immediately or refused;
	// the timeout only covers a listener whose accept backlog is full, which is
	// itself proof that something is serving.
	probeTimeout = 250 * time.Millisecond
)

// Dir returns the registry directory for a state dir: <stateDir>/instances.
func Dir(stateDir string) string {
	return filepath.Join(stateDir, dirName)
}

// ControlSocketPath returns the control socket path for an instance's state
// dir. It is the same value as control.SocketPath; see controlSocketName for
// why it is duplicated.
func ControlSocketPath(stateDir string) string {
	return filepath.Join(stateDir, controlSocketName)
}

// entryPath returns the registry file path for a (already validated) name.
func entryPath(stateDir, name string) string {
	return filepath.Join(Dir(stateDir), name+entryExt)
}

// Write publishes e to the registry, replacing any existing record for e.Name.
//
// The write is atomic and durable (see util.WriteFileAtomic): temp file in the
// registry directory, fsync, rename over the final path, directory fsync. A
// reader therefore observes either the previous record or the new one, never a
// partial file, and the rename survives a crash.
func Write(stateDir string, e Entry) error {
	if err := ValidName(e.Name); err != nil {
		return err
	}
	dir := Dir(stateDir)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("create instance registry %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return fmt.Errorf("encode instance %q: %w", e.Name, err)
	}
	data = append(data, '\n')

	if err := util.WriteFileAtomic(entryPath(stateDir, e.Name), data, entryPerm); err != nil {
		return fmt.Errorf("write instance %q: %w", e.Name, err)
	}
	return nil
}

// Read returns the registry record for name. A missing record yields an error
// wrapping fs.ErrNotExist, so callers can distinguish "not registered" from a
// real I/O failure with errors.Is.
func Read(stateDir, name string) (Entry, error) {
	if err := ValidName(name); err != nil {
		return Entry{}, err
	}
	e, err := readFile(entryPath(stateDir, name))
	if err != nil {
		return Entry{}, fmt.Errorf("instance %q: %w", name, err)
	}
	return e, nil
}

// readFile decodes one registry record, defaulting Name from the file name so
// a hand-edited entry that omits it is still usable.
func readFile(path string) (Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Entry{}, err
	}
	var e Entry
	if err := json.Unmarshal(data, &e); err != nil {
		return Entry{}, fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}
	if e.Name == "" {
		e.Name = strings.TrimSuffix(filepath.Base(path), entryExt)
	}
	return e, nil
}

// Remove deletes the registry record for name. It is idempotent: removing an
// entry that is not registered is not an error.
func Remove(stateDir, name string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	if err := os.Remove(entryPath(stateDir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove instance %q: %w", name, err)
	}
	return nil
}

// List returns every registered instance, sorted by name. An absent registry
// directory is an empty list, not an error.
//
// A record that cannot be read or decoded is skipped with a warning rather
// than failing the whole listing: one corrupt file (a half-written entry from
// an older bladerunner, say) must not hide every other running VM.
func List(stateDir string) ([]Entry, error) {
	dir := Dir(stateDir)
	des, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("list instance registry %s: %w", dir, err)
	}

	entries := make([]Entry, 0, len(des))
	for _, de := range des {
		if de.IsDir() || !strings.HasSuffix(de.Name(), entryExt) {
			continue
		}
		e, readErr := readFile(filepath.Join(dir, de.Name()))
		if readErr != nil {
			logging.L().Warn("skipping unreadable instance entry", "file", de.Name(), "error", readErr)
			continue
		}
		entries = append(entries, e)
	}
	slices.SortFunc(entries, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })
	return entries, nil
}

// Liveness is the three-value answer to "is this instance still up?".
//
// The distinction matters because the two available signals prove different
// things. Dialing the control socket proves someone is SERVING; the signal-0
// PID probe only proves a process by that number exists, which is also true of
// a holder that has not finished binding its socket, one that is wedged, and
// (rarely) of a recycled PID.
//
// Note what is deliberately NOT a signal: the control socket FILE existing on
// disk. A SIGKILLed holder leaves its socket behind, so stat'ing it reports
// "alive" forever — that lie is what kept Prune from ever reaping a crashed
// holder and made `br watch` treat a dead instance's cartridge as still held.
// Only the dial is authoritative.
type Liveness int

const (
	// Dead means nothing answers on the control socket and no process holds
	// the recorded PID. This is the only state Prune reaps.
	Dead Liveness = iota
	// ProcessOnly means the holder process exists but nothing is serving yet
	// (starting up) or any more (wedged, or shutting down). It must NOT be
	// pruned: the record belongs to a process that is still running.
	ProcessOnly
	// Serving means the control socket accepted a connection. This is the only
	// state that proves the instance is reachable.
	Serving
)

// String renders a Liveness for logs and status output.
func (l Liveness) String() string {
	switch l {
	case Serving:
		return "serving"
	case ProcessOnly:
		return "process-only"
	case Dead:
		return "dead"
	default:
		return "unknown"
	}
}

// Probe reports whether something is listening on the unix socket at
// socketPath. It is injected so the liveness ladder stays testable and so this
// package needs no dependency on internal/control (which imports this one).
type Probe func(socketPath string) bool

// DefaultProbe is the production Probe: a bounded unix-socket connect. A
// successful dial is the only proof that a holder is serving; the connection is
// closed immediately without exchanging a request, which every control listener
// tolerates.
func DefaultProbe(socketPath string) bool {
	if socketPath == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// LivenessOf reports where e sits on the liveness ladder, dialing its control
// socket with DefaultProbe.
func LivenessOf(e Entry) Liveness {
	return livenessWith(e, DefaultProbe)
}

// livenessWith is the ladder itself, with the dial injected. The strongest
// signal is tried first so a serving instance never has to fall back on a PID
// that could have been recycled.
func livenessWith(e Entry, probe Probe) Liveness {
	if e.StateDir != "" && probe(ControlSocketPath(e.StateDir)) {
		return Serving
	}
	if e.PID > 0 && processAlive(e.PID) {
		return ProcessOnly
	}
	return Dead
}

// Alive reports whether the instance is anything other than Dead — i.e. either
// serving or at least still held by a live process. It is the boolean form of
// LivenessOf for call sites that only filter; branch on LivenessOf when the
// difference between "reachable" and "merely running" matters.
func Alive(e Entry) bool {
	return LivenessOf(e) != Dead
}

// ProcessAlive reports whether pid names a live process, using the signal-0
// probe: kill(pid, 0) performs the permission and existence checks without
// delivering a signal. EPERM counts as alive (the process exists, it just
// belongs to another user).
//
// It is exported because a holder's liveness is asked about in one more place
// than its registry entry: a front end that has just SPAWNED a holder knows its
// PID and has no entry to read yet, and "the process is gone" is the difference
// between a boot still in progress and one that failed with its reason in a log
// file. Liveness of an INSTANCE is still LivenessOf's question — see LivenessOf
// and Alive; this is only the process half of it.
func ProcessAlive(pid int) bool { return processAlive(pid) }

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	return errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EPERM)
}

// Prune removes the records of instances that are Dead — nothing serving on
// their control socket AND no live holder process — and returns the names it
// removed, sorted. Use it to garbage-collect entries left behind by a holder
// that died without unregistering itself.
//
// A ProcessOnly entry is deliberately kept: its holder is still running (it may
// be mid-boot and not yet bound to its socket), and unregistering a live
// instance is far worse than leaving a stale record for a reader to reconcile.
func Prune(stateDir string) ([]string, error) {
	entries, err := List(stateDir)
	if err != nil {
		return nil, err
	}
	var removed []string
	for i := range entries {
		e := &entries[i]
		if LivenessOf(*e) != Dead {
			continue
		}
		if err := Remove(stateDir, e.Name); err != nil {
			return removed, err
		}
		removed = append(removed, e.Name)
	}
	return removed, nil
}
