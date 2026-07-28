package instance

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

// fakeProbe builds a Probe that answers serving for exactly the socket paths
// listed, so the ladder can be exercised without binding anything.
func fakeProbe(serving ...string) Probe {
	return func(socketPath string) bool {
		return slices.Contains(serving, socketPath)
	}
}

// shortTempDir returns a temp directory with a name short enough that a unix
// socket bound inside it stays under the ~104 byte sun_path limit, which
// t.TempDir's test-name-derived paths can exceed.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "brinst")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// serveControlSocket binds a real listener at stateDir's control socket path
// and accepts (and immediately drops) connections until the test ends.
func serveControlSocket(t *testing.T, stateDir string) {
	t.Helper()
	ln, err := net.Listen("unix", ControlSocketPath(stateDir))
	if err != nil {
		t.Fatalf("listen on control socket: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
	})
}

func TestLivenessLadder(t *testing.T) {
	// A leftover socket FILE from a SIGKILLed holder: it stats fine but nothing
	// is listening on it. This is the regression case — the old Alive treated
	// the file's existence as proof of life, so a crashed holder was never
	// reaped.
	staleSocketDir := t.TempDir()
	if err := os.WriteFile(ControlSocketPath(staleSocketDir), nil, 0o600); err != nil {
		t.Fatalf("create leftover socket file: %v", err)
	}

	servingDir := t.TempDir()

	tests := []struct {
		name  string
		entry Entry
		probe Probe
		want  Liveness
	}{
		{
			name:  "socket dials",
			entry: Entry{Name: "a", PID: deadPID, StateDir: servingDir},
			probe: fakeProbe(ControlSocketPath(servingDir)),
			want:  Serving,
		},
		{
			name:  "socket dials and pid is live",
			entry: Entry{Name: "b", PID: os.Getpid(), StateDir: servingDir},
			probe: fakeProbe(ControlSocketPath(servingDir)),
			want:  Serving,
		},
		{
			name:  "live pid, nothing serving",
			entry: Entry{Name: "c", PID: os.Getpid(), StateDir: servingDir},
			probe: fakeProbe(),
			want:  ProcessOnly,
		},
		{
			name:  "live pid, no state dir",
			entry: Entry{Name: "d", PID: os.Getpid()},
			probe: fakeProbe(),
			want:  ProcessOnly,
		},
		{
			name:  "leftover socket file, dead pid",
			entry: Entry{Name: "e", PID: deadPID, StateDir: staleSocketDir},
			probe: fakeProbe(),
			want:  Dead,
		},
		{
			name:  "dead pid, no socket",
			entry: Entry{Name: "f", PID: deadPID, StateDir: t.TempDir()},
			probe: fakeProbe(),
			want:  Dead,
		},
		{
			name:  "nothing at all",
			entry: Entry{Name: "g"},
			probe: fakeProbe(),
			want:  Dead,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := livenessWith(tt.entry, tt.probe); got != tt.want {
				t.Errorf("livenessWith(%+v) = %v, want %v", tt.entry, got, tt.want)
			}
			if got, want := livenessWith(tt.entry, tt.probe) != Dead, tt.want != Dead; got != want {
				t.Errorf("Alive-equivalent = %v, want %v", got, want)
			}
		})
	}
}

// The regression test proper, against the REAL probe: a leftover socket file
// belonging to a dead PID is Dead, so Prune can reap it. Before the fix
// os.Stat succeeded on that file and the entry was reported alive forever.
func TestLeftoverSocketFileWithDeadPIDIsDead(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(ControlSocketPath(dir), nil, 0o600); err != nil {
		t.Fatalf("create leftover socket file: %v", err)
	}
	e := Entry{Name: "crashed", PID: deadPID, StateDir: dir}
	if got := LivenessOf(e); got != Dead {
		t.Errorf("LivenessOf(leftover socket file, dead pid) = %v, want %v", got, Dead)
	}
	if Alive(e) {
		t.Error("Alive(leftover socket file, dead pid) = true, want false")
	}
}

// DefaultProbe must distinguish a bound socket from a plain file of the same
// name — the whole point of dialing rather than stat'ing.
func TestDefaultProbeDialsRatherThanStats(t *testing.T) {
	live := shortTempDir(t)
	serveControlSocket(t, live)
	if !DefaultProbe(ControlSocketPath(live)) {
		t.Error("DefaultProbe on a bound socket = false, want true")
	}

	stale := t.TempDir()
	if err := os.WriteFile(ControlSocketPath(stale), nil, 0o600); err != nil {
		t.Fatalf("create leftover socket file: %v", err)
	}
	if DefaultProbe(ControlSocketPath(stale)) {
		t.Error("DefaultProbe on a leftover socket file = true, want false")
	}
	if DefaultProbe(ControlSocketPath(t.TempDir())) {
		t.Error("DefaultProbe on a missing socket = true, want false")
	}
	if DefaultProbe("") {
		t.Error("DefaultProbe on an empty path = true, want false")
	}
}

func TestLivenessString(t *testing.T) {
	tests := []struct {
		liveness Liveness
		want     string
	}{
		{Serving, "serving"},
		{ProcessOnly, "process-only"},
		{Dead, "dead"},
		{Liveness(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.liveness.String(); got != tt.want {
			t.Errorf("Liveness(%d).String() = %q, want %q", int(tt.liveness), got, tt.want)
		}
	}
}

// Prune reaps only Dead entries: a serving instance and a merely-running one
// both survive, and the crash leftover that used to be immortal does not.
func TestPruneReapsOnlyDeadEntries(t *testing.T) {
	root := t.TempDir()

	serving := sampleEntry("serving-one")
	serving.PID = deadPID // liveness must come from the socket, not the PID
	serving.StateDir = shortTempDir(t)
	serveControlSocket(t, serving.StateDir)
	if err := Write(root, serving); err != nil {
		t.Fatalf("Write serving: %v", err)
	}

	processOnly := sampleEntry("process-only-one")
	processOnly.PID = os.Getpid()
	processOnly.StateDir = filepath.Join(root, "process-only") // no socket at all
	if err := Write(root, processOnly); err != nil {
		t.Fatalf("Write process-only: %v", err)
	}

	// A SIGKILLed holder: dead PID, socket FILE still on disk.
	crashed := sampleEntry("crashed-one")
	crashed.PID = deadPID
	crashed.StateDir = filepath.Join(root, "crashed")
	if err := os.MkdirAll(crashed.StateDir, 0o700); err != nil {
		t.Fatalf("mkdir crashed state dir: %v", err)
	}
	if err := os.WriteFile(ControlSocketPath(crashed.StateDir), nil, 0o600); err != nil {
		t.Fatalf("create leftover socket file: %v", err)
	}
	if err := Write(root, crashed); err != nil {
		t.Fatalf("Write crashed: %v", err)
	}

	removed, err := Prune(root)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(removed) != 1 || removed[0] != "crashed-one" {
		t.Fatalf("Prune removed %v, want [crashed-one]", removed)
	}

	remaining, err := List(root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	names := make([]string, 0, len(remaining))
	for _, e := range remaining {
		names = append(names, e.Name)
	}
	want := []string{"process-only-one", "serving-one"}
	if !slices.Equal(names, want) {
		t.Fatalf("remaining = %v, want %v", names, want)
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

// liveGroupSeconds is how long the process-group helper below is asked to live.
// The test kills it in a cleanup; the duration only has to outlast the probes.
const liveGroupSeconds = 60

// liveProcessGroup starts a child in a process group of its own and returns
// that group's id, which equals the child's PID. It is a group the test is
// certainly permitted to signal, so kill(-gid, 0) succeeds — exactly the case
// an unguarded liveness probe misreads as a live process.
//
// The test process's OWN group is not usable for this: under a container init
// the test can be process 1 in group 1, and -1 is a value Go's os package
// rejects before kill(2) ever sees it.
func liveProcessGroup(t *testing.T) int {
	t.Helper()
	child := exec.Command("sleep", strconv.Itoa(liveGroupSeconds))
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Start(); err != nil {
		t.Fatalf("start process-group helper: %v", err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	return child.Process.Pid
}

// A PID is a process, never a process group. kill(2) reads a negative argument
// as a process GROUP id: kill(-N, 0) succeeds whenever group N exists and the
// caller may signal it, so an unguarded signal-0 probe reports "alive" for a
// PID the registry could never have meant.
//
// Go's own os package rejects exactly two of these values — (*Process).Signal
// returns "process already released" for -1 and "process not initialized" for
// 0 — so those two read as dead with or without our guard. Everything at -2 and
// below reaches kill(2) unfiltered, which is why the guard has to be ours.
//
// A record misjudged as alive is unreapable: livenessWith returns ProcessOnly
// and Prune removes only Dead entries, so the entry is stranded forever.
func TestProcessAliveRejectsNonPositivePIDs(t *testing.T) {
	// spawnedDead is a PID that was real and has been reaped.
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	spawnedDead := cmd.Process.Pid

	group := liveProcessGroup(t)

	tests := []struct {
		name string
		pid  int
		want bool
	}{
		{"a live process group, negated", -group, false},
		{"minus two is a process group, not a process", -2, false},
		{"minus one is every signalable process", -1, false},
		{"zero is the caller's own process group", 0, false},
		{"the leader of that group, as a process", group, true},
		{"pid one always exists", 1, true},
		{"the test process itself", os.Getpid(), true},
		{"a pid above pid_max cannot exist", deadPID, false},
		{"a reaped child", spawnedDead, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ProcessAlive(tt.pid); got != tt.want {
				t.Errorf("ProcessAlive(%d) = %v, want %v", tt.pid, got, tt.want)
			}
		})
	}
}

// The consequence, stated as the caller sees it: a record whose PID is not a
// process must be Dead, so Prune can reap it.
func TestEntryWithAGroupPIDIsDeadAndPrunable(t *testing.T) {
	dir := t.TempDir()
	e := sampleEntry("stranded")
	e.PID = -liveProcessGroup(t)
	e.StateDir = t.TempDir() // no control socket in it
	if err := Write(dir, e); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := LivenessOf(e); got != Dead {
		t.Fatalf("LivenessOf(entry whose pid is a process group) = %v, want %v", got, Dead)
	}
	removed, err := Prune(dir)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if !slices.Contains(removed, "stranded") {
		t.Errorf("Prune removed %v, want it to contain %q", removed, "stranded")
	}
}
