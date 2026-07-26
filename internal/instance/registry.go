package instance

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/stuffbucket/bladerunner/internal/logging"
)

const (
	// dirName is the registry directory under the state dir.
	dirName = "instances"
	// entryExt is the extension of a registry record.
	entryExt = ".json"
	// tmpPattern is the os.CreateTemp pattern used for the atomic write. The
	// per-name prefix keeps concurrent writers of different names apart, and
	// the random suffix keeps concurrent writers of the SAME name apart.
	tmpPattern = entryExt + ".tmp-*"

	// dirPerm restricts the registry to its owner: entries expose socket paths
	// and working-copy locations for live VMs.
	dirPerm fs.FileMode = 0o700

	// controlSocketName mirrors internal/control.SocketName. It is duplicated
	// rather than imported because the control package is a consumer of this
	// registry; importing it here would create a cycle. Keep the two in sync.
	controlSocketName = "control.sock"
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
// The write is atomic and durable: the record is written to a temp file in the
// registry directory, fsynced, renamed over the final path, and the directory
// itself is then fsynced. A reader therefore observes either the previous
// record or the new one, never a partial file, and the rename survives a crash.
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

	tmp, err := os.CreateTemp(dir, e.Name+tmpPattern)
	if err != nil {
		return fmt.Errorf("create temp entry for %q: %w", e.Name, err)
	}
	tmpName := tmp.Name()
	if err := writeAndSync(tmp, data); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("write instance %q: %w", e.Name, err)
	}
	if err := os.Rename(tmpName, entryPath(stateDir, e.Name)); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("publish instance %q: %w", e.Name, err)
	}
	syncDir(dir)
	return nil
}

// writeAndSync writes data to f, flushes it to stable storage and closes it.
// f is closed exactly once on every path.
func writeAndSync(f *os.File, data []byte) error {
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// syncDir fsyncs a directory so a rename into it is durable. Best effort: some
// filesystems refuse to open a directory for sync, and a failure here only
// costs durability across a host crash, never correctness.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
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

// Alive reports a cheap, local estimate of whether the instance is still up.
// It is true when EITHER signal is positive:
//
//	(a) e.PID names a process that still exists (signal 0 probe), or
//	(b) a control socket exists at e.StateDir.
//
// Either alone can lie: a PID can be recycled, and a socket file survives a
// crashed holder. The disjunction is deliberately conservative — it errs
// towards "still alive" so Prune never unregisters a running VM.
//
// A caller that needs an authoritative answer must additionally DIAL the
// control socket (control.NewClient(e.StateDir).IsRunning()); that is the only
// probe that proves someone is listening.
func Alive(e Entry) bool {
	if e.PID > 0 && processAlive(e.PID) {
		return true
	}
	if e.StateDir == "" {
		return false
	}
	_, err := os.Stat(ControlSocketPath(e.StateDir))
	return err == nil
}

// processAlive reports whether pid names a live process, using the signal-0
// probe: kill(pid, 0) performs the permission and existence checks without
// delivering a signal. EPERM counts as alive (the process exists, it just
// belongs to another user).
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

// Prune removes the records of instances that are no longer Alive and returns
// the names it removed, sorted. Use it to garbage-collect entries left behind
// by a holder process that died without unregistering itself.
func Prune(stateDir string) ([]string, error) {
	entries, err := List(stateDir)
	if err != nil {
		return nil, err
	}
	var removed []string
	for i := range entries {
		e := &entries[i]
		if Alive(*e) {
			continue
		}
		if err := Remove(stateDir, e.Name); err != nil {
			return removed, err
		}
		removed = append(removed, e.Name)
	}
	return removed, nil
}
