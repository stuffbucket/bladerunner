package vmhost

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stuffbucket/bladerunner/internal/instance"
)

// deadPID is a process id that cannot be live: it is above every platform's
// pid_max, so the signal-0 probe instance.Alive uses always reports it gone.
// Using a synthetic number keeps the crash-leftover tests from depending on
// spawning and reaping a real process.
const deadPID = 1 << 30

// missingStateDir names a directory that does not exist, which is what makes an
// entry's control socket unreachable in the prune tests.
func missingStateDir(name string) string {
	return filepath.Join(os.TempDir(), "br-registry-missing", name)
}

// testEntry builds a minimal, valid registry entry for name.
func testEntry(name string) instance.Entry {
	return instance.Entry{
		Name:      name,
		Kind:      instance.KindFlat,
		StateDir:  missingStateDir(name),
		PID:       os.Getpid(),
		Ports:     instance.Ports{SSH: 6022, API: 18443},
		StartedAt: time.Unix(0, 0).UTC(),
	}
}

// A published entry is readable, and removing it takes it away again.
func TestRegistryPublishRemoveRoundTrip(t *testing.T) {
	root := t.TempDir()
	reg := newRegistry(root)
	want := testEntry("round-trip")

	if err := reg.publish(want); err != nil {
		t.Fatalf("publish: %v", err)
	}
	got, err := instance.Read(root, want.Name)
	if err != nil {
		t.Fatalf("Read after publish: %v", err)
	}
	if got != want {
		t.Fatalf("read back %+v, want %+v", got, want)
	}

	if err := reg.remove(); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := instance.Read(root, want.Name); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Read after remove = %v, want a not-exist error", err)
	}
}

// Republishing an unchanged entry must not rewrite the file: the Host calls it
// after several steps, and only the ones that actually changed something should
// cost a write. Deleting the file behind the registry's back is how the test
// observes the elision — a gated publish leaves it deleted.
func TestRegistryPublishIsChangeGated(t *testing.T) {
	root := t.TempDir()
	reg := newRegistry(root)
	entry := testEntry("gated")

	if err := reg.publish(entry); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	path := filepath.Join(instance.Dir(root), entry.Name+".json")
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove entry file: %v", err)
	}

	if err := reg.publish(entry); err != nil {
		t.Fatalf("republish unchanged: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unchanged republish rewrote the entry (stat = %v)", err)
	}

	// A genuine change must still land.
	entry.Ports.SSH = 7022
	if err := reg.publish(entry); err != nil {
		t.Fatalf("publish changed: %v", err)
	}
	got, err := instance.Read(root, entry.Name)
	if err != nil {
		t.Fatalf("Read after change: %v", err)
	}
	if got.Ports.SSH != 7022 {
		t.Fatalf("ssh port = %d, want 7022", got.Ports.SSH)
	}
}

// A name that cannot be a registry file name is reported, not written, and
// never fails anything else.
func TestRegistryPublishRejectsInvalidName(t *testing.T) {
	root := t.TempDir()
	reg := newRegistry(root)

	// The Finder mount-collision case: "bladerunner-demo 1".
	err := reg.publish(testEntry("bladerunner-demo 1"))
	if !errors.Is(err, instance.ErrInvalidName) {
		t.Fatalf("publish invalid name = %v, want ErrInvalidName", err)
	}
	entries, listErr := instance.List(root)
	if listErr != nil {
		t.Fatalf("List: %v", listErr)
	}
	if len(entries) != 0 {
		t.Fatalf("registry has %d entries, want none", len(entries))
	}
}

// remove before publish, and remove twice, are both no-ops.
func TestRegistryRemoveIsIdempotent(t *testing.T) {
	reg := newRegistry(t.TempDir())
	if err := reg.remove(); err != nil {
		t.Fatalf("remove before publish: %v", err)
	}
	if err := reg.publish(testEntry("idempotent")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := reg.remove(); err != nil {
		t.Fatalf("first remove: %v", err)
	}
	if err := reg.remove(); err != nil {
		t.Fatalf("second remove: %v", err)
	}
}

// A registry with no root is inert, so a caller with no host state directory
// need not branch.
func TestRegistryWithoutRootIsInert(t *testing.T) {
	reg := newRegistry("")
	if err := reg.publish(testEntry("inert")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := reg.remove(); err != nil {
		t.Fatalf("remove: %v", err)
	}
	reg.prune() // must not panic
}

// A nil registry is inert too: teardown can run before startRegistry did.
func TestNilRegistryIsInert(t *testing.T) {
	var reg *registry
	if err := reg.publish(testEntry("nil")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := reg.remove(); err != nil {
		t.Fatalf("remove: %v", err)
	}
	reg.prune()
}

// The crash case: a holder died without retracting its entry. Its record has a
// PID that is gone and a state dir with no control socket, so prune collects it
// — while a live neighbour's entry is left strictly alone.
func TestRegistryPrunesCrashLeftovers(t *testing.T) {
	root := t.TempDir()

	dead := testEntry("crashed")
	dead.PID = deadPID
	dead.StateDir = filepath.Join(root, "gone") // no control socket there
	if err := instance.Write(root, dead); err != nil {
		t.Fatalf("write dead entry: %v", err)
	}

	live := testEntry("live")
	live.PID = os.Getpid()
	if err := instance.Write(root, live); err != nil {
		t.Fatalf("write live entry: %v", err)
	}

	newRegistry(root).prune()

	entries, err := instance.List(root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "live" {
		t.Fatalf("after prune the registry holds %+v, want only the live entry", entries)
	}
}

// A crash leftover whose control socket file still exists is NOT pruned:
// instance.Alive is deliberately conservative, because unregistering a running
// VM is far worse than leaving a stale record for a reader to reconcile.
func TestRegistryPruneKeepsEntriesWithASocket(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "with-socket")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(instance.ControlSocketPath(stateDir), nil, 0o600); err != nil {
		t.Fatalf("write socket placeholder: %v", err)
	}

	e := testEntry("with-socket")
	e.PID = deadPID
	e.StateDir = stateDir
	if err := instance.Write(root, e); err != nil {
		t.Fatalf("write entry: %v", err)
	}

	newRegistry(root).prune()

	if _, err := instance.Read(root, e.Name); err != nil {
		t.Fatalf("entry with a live socket was pruned: %v", err)
	}
}

// The Host wiring: startRegistry publishes what Info reports, and stopRegistry
// retracts it. RegistryRoot is redirected at the env var so the test never
// touches the real state dir.
func TestHostPublishesAndRetractsItsEntry(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BLADERUNNER_STATE_DIR", root)

	host, err := New(Spec{Kind: instance.KindFlat, Name: "holder-test", BinaryVersion: "test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := host.startRegistry(); err != nil {
		t.Fatalf("startRegistry: %v", err)
	}

	got, err := instance.Read(root, "holder-test")
	if err != nil {
		t.Fatalf("Read published entry: %v", err)
	}
	if got.PID != os.Getpid() {
		t.Fatalf("published PID = %d, want %d", got.PID, os.Getpid())
	}
	if got.BinaryVersion != "test" {
		t.Fatalf("published BinaryVersion = %q, want %q", got.BinaryVersion, "test")
	}

	if err := host.stopRegistry(); err != nil {
		t.Fatalf("stopRegistry: %v", err)
	}
	if _, err := instance.Read(root, "holder-test"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("entry survived stopRegistry: %v", err)
	}
}

// RegistryRoot is the host's own state dir, not the instance's: a cartridge
// rooted under /Volumes must still publish somewhere a manager can find.
func TestRegistryRootFollowsTheHostStateDir(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BLADERUNNER_STATE_DIR", root)
	if got := RegistryRoot(); got != root {
		t.Fatalf("RegistryRoot() = %q, want %q", got, root)
	}
}
