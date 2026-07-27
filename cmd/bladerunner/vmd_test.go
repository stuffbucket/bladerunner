package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/instance"
	"github.com/stuffbucket/bladerunner/internal/vmhost"
)

// signalBudget bounds how long a test waits for the signal loop to react.
const signalBudget = 2 * time.Second

// setVMDFlags installs holder flag values for one test and restores the
// previous ones afterwards, because the flags are package-level cobra targets.
func setVMDFlags(t *testing.T, stateDir, cartridgePath, name string, gui bool, drain time.Duration) {
	t.Helper()
	saved := vmdFlags
	t.Cleanup(func() { vmdFlags = saved })
	vmdFlags.stateDir = stateDir
	vmdFlags.cartridgePath = cartridgePath
	vmdFlags.name = name
	vmdFlags.gui = gui
	vmdFlags.drainTimeout = drain
}

// The one thing a holder cannot default is which instance it holds, and it must
// say so clearly rather than silently adopting the default state dir.
func TestBuildVMDSpecRequiresStateDir(t *testing.T) {
	setVMDFlags(t, "", "", "", false, 0)
	_, err := buildVMDSpec()
	if !errors.Is(err, errVMDStateDirRequired) {
		t.Fatalf("buildVMDSpec() error = %v, want errVMDStateDirRequired", err)
	}
	if !strings.Contains(err.Error(), "--state-dir") {
		t.Fatalf("error %q does not name the missing flag", err)
	}
}

// A plain holder is a flat instance rooted at the state dir it was pointed at,
// and the Spec it builds must be one vmhost will accept.
func TestBuildVMDSpecFlat(t *testing.T) {
	setVMDFlags(t, "/tmp/br-holder", "", "", false, 90*time.Second)

	spec, err := buildVMDSpec()
	if err != nil {
		t.Fatalf("buildVMDSpec: %v", err)
	}
	if spec.Kind != instance.KindFlat {
		t.Fatalf("Kind = %q, want %q", spec.Kind, instance.KindFlat)
	}
	if spec.StateDir != "/tmp/br-holder" {
		t.Fatalf("StateDir = %q", spec.StateDir)
	}
	if spec.CartridgePath != "" || spec.Mountpoint != "" {
		t.Fatalf("a flat holder must carry no cartridge fields: %+v", spec)
	}
	if spec.DrainTimeout != 90*time.Second {
		t.Fatalf("DrainTimeout = %v, want 90s", spec.DrainTimeout)
	}
	if err := spec.Validate(); err != nil {
		t.Fatalf("the spec a holder builds must be runnable: %v", err)
	}
}

// A cartridge holder derives its mountpoint so two cartridges never land on top
// of each other, and it still validates.
func TestBuildVMDSpecCartridge(t *testing.T) {
	setVMDFlags(t, "/tmp/br-holder", "/Volumes/x/demo.dmg", "", false, 0)

	spec, err := buildVMDSpec()
	if err != nil {
		t.Fatalf("buildVMDSpec: %v", err)
	}
	if spec.Kind != instance.KindCartridge {
		t.Fatalf("Kind = %q, want %q", spec.Kind, instance.KindCartridge)
	}
	if spec.CartridgePath != "/Volumes/x/demo.dmg" {
		t.Fatalf("CartridgePath = %q", spec.CartridgePath)
	}
	if !strings.HasSuffix(spec.Mountpoint, "demo") {
		t.Fatalf("Mountpoint = %q, want it derived from the cartridge name", spec.Mountpoint)
	}
	if err := spec.Validate(); err != nil {
		t.Fatalf("the spec a cartridge holder builds must be runnable: %v", err)
	}
}

// An explicit --name wins over the cartridge's own basename for both the
// instance name and the mount slot.
func TestBuildVMDSpecNameOverridesTheMountSlot(t *testing.T) {
	setVMDFlags(t, "/tmp/br-holder", "/Volumes/x/demo.dmg", "second", false, 0)

	spec, err := buildVMDSpec()
	if err != nil {
		t.Fatalf("buildVMDSpec: %v", err)
	}
	if spec.Name != "second" {
		t.Fatalf("Name = %q, want %q", spec.Name, "second")
	}
	if !strings.HasSuffix(spec.Mountpoint, "second") {
		t.Fatalf("Mountpoint = %q, want it to use the explicit name", spec.Mountpoint)
	}
}

// --gui is only asserted when it was passed, so a holder cannot clobber the
// instance's persisted Settings with a flag default.
func TestBuildVMDSpecOnlyAssertsGUIWhenSet(t *testing.T) {
	setVMDFlags(t, "/tmp/br-holder", "", "", false, 0)
	spec, err := buildVMDSpec()
	if err != nil {
		t.Fatalf("buildVMDSpec: %v", err)
	}
	if len(spec.ChangedFlags) != 0 {
		t.Fatalf("ChangedFlags = %v, want none", spec.ChangedFlags)
	}

	setVMDFlags(t, "/tmp/br-holder", "", "", true, 0)
	spec, err = buildVMDSpec()
	if err != nil {
		t.Fatalf("buildVMDSpec: %v", err)
	}
	if len(spec.ChangedFlags) != 1 || spec.ChangedFlags[0] != "gui" {
		t.Fatalf("ChangedFlags = %v, want [gui]", spec.ChangedFlags)
	}
	if !spec.Overrides.GUI {
		t.Fatal("Overrides.GUI must follow the flag")
	}
}

// The holder must never appear in `br --help`.
func TestVMDCommandIsHidden(t *testing.T) {
	if !vmdCmd.Hidden {
		t.Fatal("br vmd must be hidden: it is spawned by bladerunner, not by users")
	}
	if vmdCmd.IsAvailableCommand() {
		t.Fatal("br vmd must not be an available command: cobra would list it in help")
	}
	found := false
	for _, c := range rootCmd.Commands() {
		if c.Name() == "vmd" {
			found = true
		}
	}
	if !found {
		t.Fatal("br vmd is not registered on the root command")
	}
}

// fakeDrainer stands in for *vmhost.Host in the signal loop: it records the
// calls and can block for as long as a test needs.
type fakeDrainer struct {
	calls   atomic.Int64
	timeout atomic.Int64
	block   chan struct{}
	err     error
}

func (f *fakeDrainer) Drain(_ context.Context, timeout time.Duration) error {
	f.calls.Add(1)
	f.timeout.Store(int64(timeout))
	if f.block != nil {
		<-f.block
	}
	return f.err
}

// The first SIGTERM means ORDERLY EJECT: it drains with the configured budget
// and does NOT cancel the run context, because canceling would tear the host
// down with the guest still running.
func TestVMDSignalLoopFirstSignalDrainsRatherThanCancelling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	drainer := &fakeDrainer{block: make(chan struct{})}
	defer close(drainer.block)

	var escalated atomic.Int64
	signals := make(chan os.Signal, vmdSignalBuffer)
	go vmdSignalLoop(ctx, signals, drainer, func() { escalated.Add(1) }, 42*time.Second)

	signals <- syscall.SIGTERM
	waitForInt(t, &drainer.calls, 1, signalBudget, "SIGTERM did not start a drain")

	if got := time.Duration(drainer.timeout.Load()); got != 42*time.Second {
		t.Fatalf("drain timeout = %v, want 42s", got)
	}
	if got := escalated.Load(); got != 0 {
		t.Fatalf("a single SIGTERM escalated %d times; it must only drain", got)
	}
	if ctx.Err() != nil {
		t.Fatal("a single SIGTERM canceled the run context instead of draining")
	}
}

// A second SIGTERM while the drain is still in flight is an explicit
// escalation: it releases Run so teardown can force the VMM down.
func TestVMDSignalLoopSecondSignalEscalates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	drainer := &fakeDrainer{block: make(chan struct{})}
	defer close(drainer.block)

	var escalated atomic.Int64
	signals := make(chan os.Signal, vmdSignalBuffer)
	go vmdSignalLoop(ctx, signals, drainer, func() { escalated.Add(1) }, time.Second)

	signals <- syscall.SIGTERM
	waitForInt(t, &drainer.calls, 1, signalBudget, "the first SIGTERM did not start a drain")

	signals <- syscall.SIGTERM
	waitForInt(t, &escalated, 1, signalBudget, "the second SIGTERM did not escalate")

	if got := drainer.calls.Load(); got != 1 {
		t.Fatalf("drain ran %d times, want exactly 1", got)
	}
}

// The loop exits when the run context is done, so it never outlives the host.
func TestVMDSignalLoopExitsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		vmdSignalLoop(ctx, make(chan os.Signal), &fakeDrainer{}, cancel, time.Second)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(signalBudget):
		t.Fatal("the signal loop outlived its context")
	}
}

// A drain with nothing to drain still ends in teardown: the holder must not sit
// there holding a cartridge after being asked to stop.
func TestVMDDrainReleasesWhenThereIsNothingToDrain(t *testing.T) {
	var released atomic.Int64
	vmdDrain(context.Background(), &fakeDrainer{err: vmhost.ErrNotStarted}, func() { released.Add(1) }, time.Second)
	if got := released.Load(); got != 1 {
		t.Fatalf("release ran %d times, want 1", got)
	}
}

// A failed drain likewise falls through to teardown rather than wedging.
func TestVMDDrainReleasesOnFailure(t *testing.T) {
	var released atomic.Int64
	vmdDrain(context.Background(), &fakeDrainer{err: errors.New("boom")}, func() { released.Add(1) }, time.Second)
	if got := released.Load(); got != 1 {
		t.Fatalf("release ran %d times, want 1", got)
	}
}

// A successful drain releases Run itself, so the loop must not double-release.
func TestVMDDrainLeavesReleaseToDrainOnSuccess(t *testing.T) {
	var released atomic.Int64
	vmdDrain(context.Background(), &fakeDrainer{}, func() { released.Add(1) }, time.Second)
	if got := released.Load(); got != 0 {
		t.Fatalf("release ran %d times after a clean drain, want 0", got)
	}
}

// The holder log is per-instance, created private, and appended to — a restart
// must not throw away the log of the boot that failed.
func TestOpenVMDLogAppendsPrivately(t *testing.T) {
	dir := t.TempDir()
	if got, want := vmdLogPath(dir, ""), filepath.Join(dir, "vmd.log"); got != want {
		t.Fatalf("vmdLogPath = %q, want %q", got, want)
	}

	first, err := openVMDLog(dir, "")
	if err != nil {
		t.Fatalf("openVMDLog: %v", err)
	}
	if _, err := first.WriteString("one\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second, err := openVMDLog(dir, "")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := second.WriteString("two\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	data, err := os.ReadFile(vmdLogPath(dir, ""))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if string(data) != "one\ntwo\n" {
		t.Fatalf("log = %q, want the second open to have appended", data)
	}

	info, err := os.Stat(vmdLogPath(dir, ""))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("log mode = %v, want 0600", perm)
	}
}

// TestHoldersDoNotShareALogFile is the resource-leak regression. Every holder
// used to be spawned with the registry ROOT as its state dir, so every one of
// them appended to a single ~/.local/state/bladerunner/vmd.log: two running
// holders interleaved their output into one file that nothing ever truncated.
func TestHoldersDoNotShareALogFile(t *testing.T) {
	root := t.TempDir()

	demo := vmdLogPath(root, "demo")
	other := vmdLogPath(root, "other")
	if demo == other {
		t.Fatalf("two instances share the holder log %q", demo)
	}
	// Both still live under the directory they were spawned with.
	for _, path := range []string{demo, other} {
		if filepath.Dir(path) != root {
			t.Errorf("holder log %q escaped %q", path, root)
		}
	}
	// The flat default keeps the historical name.
	if got, want := vmdLogPath(root, config.DefaultInstanceName), filepath.Join(root, "vmd.log"); got != want {
		t.Errorf("default holder log = %q, want %q", got, want)
	}
	// A name that is not a usable path element must not be pasted into one.
	if got := vmdLogPath(root, "../../etc/passwd"); filepath.Dir(got) != root {
		t.Errorf("holder log %q escaped %q", got, root)
	}

	// And the name a spawn derives is what separates the cartridge holders the
	// mount watcher starts, all of which share the registry root.
	spawns := []holderSpawn{
		{StateDir: root, CartridgePath: "/Users/me/Downloads/demo.dmg"},
		{StateDir: root, CartridgePath: "/Users/me/Downloads/other.dmg"},
		{StateDir: root, Name: "named", CartridgePath: "/Users/me/Downloads/demo.dmg"},
	}
	seen := map[string]bool{}
	for _, s := range spawns {
		path := vmdLogPath(s.StateDir, s.logName())
		if seen[path] {
			t.Errorf("holder spawn %+v reuses the log %q", s, path)
		}
		seen[path] = true
	}
}

// The holder log is capped: a holder writes to it for as long as the VM runs,
// and nothing else ever truncates it.
func TestOpenVMDLogRotatesAnOversizedLog(t *testing.T) {
	dir := t.TempDir()
	path := vmdLogPath(dir, "demo")
	oversized := make([]byte, vmdLogMaxSizeMB*1024*1024+1)
	if err := os.WriteFile(path, oversized, 0o600); err != nil {
		t.Fatalf("stage oversized log: %v", err)
	}

	f, err := openVMDLog(dir, "demo")
	if err != nil {
		t.Fatalf("openVMDLog: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("holder log = %d bytes, want a freshly rotated (empty) file", info.Size())
	}
	// The rotated generation is kept beside it, so the previous boot's output
	// survives the rotation.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 {
		t.Fatalf("rotation discarded the previous log: %v", entries)
	}
}

// waitForInt polls a counter until it reaches want, failing with msg on timeout.
func waitForInt(t *testing.T, counter *atomic.Int64, want int64, budget time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if counter.Load() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(msg)
}
