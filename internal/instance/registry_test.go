package instance

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// deadPID is a PID that is essentially certain not to be running: PID 1 is
// init, and picking a value above the default pid_max on both macOS and Linux
// guarantees FindProcess+signal fails.
const deadPID = 4194305

func sampleEntry(name string) Entry {
	return Entry{
		Name:            name,
		Kind:            KindCartridge,
		StateDir:        "/Volumes/" + name,
		SourcePath:      "/Users/someone/" + name + ".dmg",
		WorkingCopy:     "/Volumes/" + name + "/disk.img",
		DevNode:         "/dev/disk7",
		Mountpoint:      "/Volumes/" + name,
		PID:             deadPID,
		Ports:           Ports{SSH: 6022, API: 18443, Web: 18444, OIDC: 15556, NTP: 10123},
		ProtocolVersion: 3,
		BinaryVersion:   "v1.2.3",
		StartedAt:       time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC),
		GUI:             true,
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := sampleEntry("cart-one")

	if err := Write(dir, want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Read(dir, want.Name)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !got.StartedAt.Equal(want.StartedAt) {
		t.Errorf("StartedAt = %v, want %v", got.StartedAt, want.StartedAt)
	}
	got.StartedAt = want.StartedAt // compared above; normalize monotonic/location
	if got != want {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestWriteCreatesPrivateDirAndNoTempLeftBehind(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"alpha", "beta"} {
		if err := Write(dir, sampleEntry(name)); err != nil {
			t.Fatalf("Write(%s): %v", name, err)
		}
	}

	info, err := os.Stat(Dir(dir))
	if err != nil {
		t.Fatalf("stat registry dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("registry dir perm = %o, want 700", perm)
	}

	des, err := os.ReadDir(Dir(dir))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(des) != 2 {
		t.Fatalf("registry has %d files, want 2: %v", len(des), des)
	}
	for _, de := range des {
		if strings.Contains(de.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", de.Name())
		}
	}
}

func TestWriteOverwritesExistingEntry(t *testing.T) {
	dir := t.TempDir()
	e := sampleEntry("alpha")
	if err := Write(dir, e); err != nil {
		t.Fatalf("Write: %v", err)
	}
	e.BinaryVersion = "v9.9.9"
	if err := Write(dir, e); err != nil {
		t.Fatalf("Write (overwrite): %v", err)
	}

	got, err := Read(dir, "alpha")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.BinaryVersion != "v9.9.9" {
		t.Errorf("BinaryVersion = %q, want v9.9.9", got.BinaryVersion)
	}
	des, err := os.ReadDir(Dir(dir))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(des) != 1 {
		t.Errorf("registry has %d files after overwrite, want 1", len(des))
	}
}

func TestReadMissingIsNotExist(t *testing.T) {
	dir := t.TempDir()
	if _, err := Read(dir, "nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Read of missing entry: err = %v, want fs.ErrNotExist", err)
	}
}

func TestRemoveIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, sampleEntry("gone")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	for i := range 3 {
		if err := Remove(dir, "gone"); err != nil {
			t.Fatalf("Remove #%d: %v", i, err)
		}
	}
	if _, err := Read(dir, "gone"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("entry still present after Remove: %v", err)
	}
}

func TestRemoveOnMissingRegistryDir(t *testing.T) {
	if err := Remove(t.TempDir(), "never-written"); err != nil {
		t.Fatalf("Remove with no registry dir: %v", err)
	}
}

func TestListEmptyRegistry(t *testing.T) {
	got, err := List(t.TempDir())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List = %v, want empty", got)
	}
}

func TestListSkipsCorruptEntries(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"charlie", "alpha"} {
		if err := Write(dir, sampleEntry(name)); err != nil {
			t.Fatalf("Write(%s): %v", name, err)
		}
	}
	// A deliberately corrupt record, plus a non-JSON file that must be ignored.
	if err := os.WriteFile(filepath.Join(Dir(dir), "bravo.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt entry: %v", err)
	}
	if err := os.WriteFile(filepath.Join(Dir(dir), "README.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatalf("write stray file: %v", err)
	}

	got, err := List(dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	names := make([]string, 0, len(got))
	for _, e := range got {
		names = append(names, e.Name)
	}
	want := []string{"alpha", "charlie"}
	if len(names) != len(want) {
		t.Fatalf("List names = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("List names = %v, want %v (sorted)", names, want)
		}
	}
}

func TestValidName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"simple", "default", false},
		{"dashed", "my-cartridge-2", false},
		{"digits", "9lives", false},
		{"max length", strings.Repeat("a", MaxNameLen), false},
		{"empty", "", true},
		{"dot", ".", true},
		{"dotdot", "..", true},
		{"slash", "a/b", true},
		{"leading slash", "/abs", true},
		{"traversal", "../escape", true},
		{"uppercase", "Alpha", true},
		{"space", "my cart", true},
		{"leading dash", "-lead", true},
		{"too long", strings.Repeat("a", MaxNameLen+1), true},
		{"nul", "a\x00b", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidName(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidName(%q) = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalidName) {
				t.Errorf("ValidName(%q) error does not wrap ErrInvalidName: %v", tt.input, err)
			}
		})
	}
}

func TestWriteRejectsInvalidName(t *testing.T) {
	dir := t.TempDir()
	e := sampleEntry("ok")
	e.Name = "../escape"
	if err := Write(dir, e); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("Write with traversal name: err = %v, want ErrInvalidName", err)
	}
}

func TestKindValid(t *testing.T) {
	tests := []struct {
		kind Kind
		want bool
	}{
		{KindFlat, true},
		{KindDisk, true},
		{KindCartridge, true},
		{Kind(""), false},
		{Kind("tape"), false},
	}
	for _, tt := range tests {
		if got := tt.kind.Valid(); got != tt.want {
			t.Errorf("Kind(%q).Valid() = %v, want %v", tt.kind, got, tt.want)
		}
	}
}

func TestAlive(t *testing.T) {
	socketDir := t.TempDir()
	if err := os.WriteFile(ControlSocketPath(socketDir), nil, 0o600); err != nil {
		t.Fatalf("create fake socket: %v", err)
	}

	tests := []struct {
		name  string
		entry Entry
		want  bool
	}{
		{"live pid", Entry{Name: "a", PID: os.Getpid()}, true},
		{"dead pid, no socket", Entry{Name: "b", PID: deadPID, StateDir: t.TempDir()}, false},
		{"no pid, socket present", Entry{Name: "c", StateDir: socketDir}, true},
		{"dead pid, socket present", Entry{Name: "d", PID: deadPID, StateDir: socketDir}, true},
		{"nothing at all", Entry{Name: "e"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Alive(tt.entry); got != tt.want {
				t.Errorf("Alive(%+v) = %v, want %v", tt.entry, got, tt.want)
			}
		})
	}
}

func TestPruneRemovesDeadKeepsLive(t *testing.T) {
	dir := t.TempDir()

	dead := sampleEntry("dead-one")
	dead.StateDir = filepath.Join(dir, "dead") // no control socket there
	if err := Write(dir, dead); err != nil {
		t.Fatalf("Write dead: %v", err)
	}

	live := sampleEntry("live-one")
	live.PID = os.Getpid()
	live.StateDir = filepath.Join(dir, "live")
	if err := Write(dir, live); err != nil {
		t.Fatalf("Write live: %v", err)
	}

	removed, err := Prune(dir)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(removed) != 1 || removed[0] != "dead-one" {
		t.Fatalf("Prune removed %v, want [dead-one]", removed)
	}

	remaining, err := List(dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(remaining) != 1 || remaining[0].Name != "live-one" {
		t.Fatalf("remaining = %v, want [live-one]", remaining)
	}

	// Pruning again is a no-op.
	removed, err = Prune(dir)
	if err != nil {
		t.Fatalf("Prune (second): %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("second Prune removed %v, want none", removed)
	}
}

func TestConcurrentWriteDistinctNames(t *testing.T) {
	dir := t.TempDir()
	const writers = 16

	var wg sync.WaitGroup
	errCh := make(chan error, writers)
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e := sampleEntry(nameForIndex(i))
			if err := Write(dir, e); err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent Write: %v", err)
	}

	got, err := List(dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != writers {
		t.Fatalf("List returned %d entries, want %d", len(got), writers)
	}
}

// nameForIndex builds a registry-legal name from a loop index.
func nameForIndex(i int) string {
	return "inst-" + strings.Repeat("x", i%4) + string(rune('a'+i))
}
