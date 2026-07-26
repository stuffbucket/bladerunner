package vmhost

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stuffbucket/bladerunner/internal/cartridge"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/control"
	"github.com/stuffbucket/bladerunner/internal/diskarb"
	"github.com/stuffbucket/bladerunner/internal/instance"
)

// recorder collects the start/stop events a stepStack produces, which is what
// the ordering assertions below are made against.
type recorder struct {
	events []string
}

func (r *recorder) log(event string) { r.events = append(r.events, event) }

// fakeStep builds a step that records its start and stop, optionally failing to
// start.
func fakeStep(r *recorder, name string, startErr error) step {
	return step{
		name: name,
		start: func(context.Context) error {
			r.log("start:" + name)
			return startErr
		},
		stop: func() error {
			r.log("stop:" + name)
			return nil
		},
	}
}

func joined(events []string) string { return strings.Join(events, ",") }

// A clean run starts every step in order and tears them down in exact reverse.
func TestStepStackTeardownIsReverseOrder(t *testing.T) {
	r := &recorder{}
	var stack stepStack
	steps := []step{
		fakeStep(r, "one", nil),
		fakeStep(r, "two", nil),
		fakeStep(r, "three", nil),
	}

	if err := stack.run(context.Background(), steps, nil); err != nil {
		t.Fatalf("run() = %v, want nil", err)
	}
	if got, want := joined(r.events), "start:one,start:two,start:three"; got != want {
		t.Fatalf("start order = %q, want %q", got, want)
	}

	stack.teardown(nil)
	want := "start:one,start:two,start:three,stop:three,stop:two,stop:one"
	if got := joined(r.events); got != want {
		t.Fatalf("teardown order = %q, want %q", got, want)
	}
}

// A failure at step N tears down exactly steps 1..N-1 — the failing step is
// never stopped (a start that fails owns its own cleanup) and later steps never
// run at all.
func TestStepStackFailureUnwindsOnlyStartedSteps(t *testing.T) {
	r := &recorder{}
	var stack stepStack
	boom := errors.New("boom")
	steps := []step{
		fakeStep(r, "one", nil),
		fakeStep(r, "two", nil),
		fakeStep(r, "three", boom),
		fakeStep(r, "four", nil),
	}

	err := stack.run(context.Background(), steps, nil)
	if !errors.Is(err, boom) {
		t.Fatalf("run() = %v, want %v", err, boom)
	}
	want := "start:one,start:two,start:three,stop:two,stop:one"
	if got := joined(r.events); got != want {
		t.Fatalf("unwind = %q, want %q", got, want)
	}
}

// Teardown is idempotent: run's own unwind plus a caller's deferred teardown
// must not stop anything twice.
func TestStepStackTeardownIsIdempotent(t *testing.T) {
	r := &recorder{}
	var stack stepStack
	steps := []step{fakeStep(r, "one", nil), fakeStep(r, "two", nil)}

	if err := stack.run(context.Background(), steps, nil); err != nil {
		t.Fatal(err)
	}
	stack.teardown(nil)
	before := joined(r.events)
	stack.teardown(nil)
	stack.teardown(nil)
	if got := joined(r.events); got != before {
		t.Fatalf("repeated teardown produced %q, want it unchanged at %q", got, before)
	}
}

// A failed run's unwind and a later deferred teardown likewise cooperate.
func TestStepStackFailedRunThenTeardownIsIdempotent(t *testing.T) {
	r := &recorder{}
	var stack stepStack
	steps := []step{fakeStep(r, "one", nil), fakeStep(r, "two", errors.New("boom"))}

	_ = stack.run(context.Background(), steps, nil)
	before := joined(r.events)
	stack.teardown(nil)
	if got := joined(r.events); got != before {
		t.Fatalf("teardown after a failed run produced %q, want it unchanged at %q", got, before)
	}
}

// Steps with no stop closure are skipped by teardown without disturbing the
// order of the ones that have one.
func TestStepStackSkipsStepsWithoutStop(t *testing.T) {
	r := &recorder{}
	var stack stepStack
	steps := []step{
		fakeStep(r, "one", nil),
		{name: "two", start: func(context.Context) error { r.log("start:two"); return nil }},
		fakeStep(r, "three", nil),
	}

	if err := stack.run(context.Background(), steps, nil); err != nil {
		t.Fatal(err)
	}
	stack.teardown(nil)
	want := "start:one,start:two,start:three,stop:three,stop:one"
	if got := joined(r.events); got != want {
		t.Fatalf("teardown = %q, want %q", got, want)
	}
}

// Teardown failures are reported per step and never abort the unwind.
func TestStepStackReportsStopErrors(t *testing.T) {
	var stack stepStack
	failing := errors.New("detach failed")
	stopped := []string{}
	steps := []step{
		{
			name:  "one",
			start: func(context.Context) error { return nil },
			stop:  func() error { stopped = append(stopped, "one"); return nil },
		},
		{
			name:  "two",
			start: func(context.Context) error { return nil },
			stop:  func() error { stopped = append(stopped, "two"); return failing },
		},
	}
	if err := stack.run(context.Background(), steps, nil); err != nil {
		t.Fatal(err)
	}

	var gotStep string
	var gotErr error
	stack.teardown(func(name string, err error) { gotStep, gotErr = name, err })

	if gotStep != "two" || !errors.Is(gotErr, failing) {
		t.Fatalf("stop error reported as (%q, %v), want (\"two\", %v)", gotStep, gotErr, failing)
	}
	if got := strings.Join(stopped, ","); got != "two,one" {
		t.Fatalf("stopped = %q, want %q (the failure must not abort the unwind)", got, "two,one")
	}
}

// The real Host's step list is what the ordering guarantee applies to, so pin
// its order: the cartridge is attached first and detached last, the unmount
// veto is registered just after the mount exists and unregistered just before
// it goes away, the registry entry is published as soon as the control socket
// answers, and the VM stops before the ports it bound are released.
func TestHostStepOrder(t *testing.T) {
	t.Setenv(config.ForceHostedImageEnvVar, "")
	t.Setenv(config.ForceDebianImageEnvVar, "")

	h, err := New(Spec{Kind: instance.KindFlat})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(h.steps()))
	for _, s := range h.steps() {
		got = append(got, s.name)
	}
	want := []string{
		StepCartridge, StepUnmountVeto, StepControl, StepServe, StepRegistry,
		StepConfig, StepPorts, StepSSHKeys,
		StepOIDC, StepNTP, StepRunner, StepBootStage, StepVM, StepWebProxy,
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("steps = %v, want %v", got, want)
	}
}

// Two Hosts must be constructible in one process: nothing about a Host lives in
// a package-level variable.
func TestTwoHostsAreIndependent(t *testing.T) {
	t.Setenv(config.ForceHostedImageEnvVar, "")
	t.Setenv(config.ForceDebianImageEnvVar, "")

	a, err := New(Spec{Kind: instance.KindFlat, Name: "alpha", StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(Spec{Kind: instance.KindFlat, Name: "beta", StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if a.Info().Name != "alpha" || b.Info().Name != "beta" {
		t.Fatalf("hosts share state: %q / %q", a.Info().Name, b.Info().Name)
	}
	if a.spec.StateDir == b.spec.StateDir {
		t.Fatal("hosts share a state dir")
	}
}

// Info reports what the registry needs even before anything has started.
func TestHostInfoBeforeStart(t *testing.T) {
	t.Setenv(config.ForceHostedImageEnvVar, "")
	t.Setenv(config.ForceDebianImageEnvVar, "")

	h, err := New(Spec{
		Kind:          instance.KindCartridge,
		Name:          "demo",
		CartridgePath: "/tmp/demo.dmg",
		Mountpoint:    "/state/mnt/demo",
		BinaryVersion: "v1.2.3",
		Driven:        true,
		Overrides: Overrides{
			CPUs:        config.DefaultCPUs,
			MemoryGiB:   config.DefaultMemoryGiB,
			DiskSizeGiB: config.DefaultDiskSizeGiB,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	e := h.Info()
	if e.Name != "demo" || e.Kind != instance.KindCartridge {
		t.Errorf("identity = %q/%q", e.Name, e.Kind)
	}
	if e.SourcePath != "/tmp/demo.dmg" {
		t.Errorf("SourcePath = %q", e.SourcePath)
	}
	if e.BinaryVersion != "v1.2.3" {
		t.Errorf("BinaryVersion = %q", e.BinaryVersion)
	}
	if e.ProtocolVersion != control.ProtocolVersion {
		t.Errorf("ProtocolVersion = %d, want %d", e.ProtocolVersion, control.ProtocolVersion)
	}
	if e.PID == 0 {
		t.Error("PID = 0, want this process")
	}
}

// A cartridge instance is published under the cartridge's OWN name, not the
// basename of the directory it happens to be mounted at.
//
// This is the browsable-mount regression: macOS roots a cartridge's state dir
// at /Volumes/bladerunner-demo, so the old fallback (filepath.Base of the state
// dir) registered the instance as "bladerunner-demo" — and `br eject demo`,
// `--instance demo` and the ssh alias all missed it.
func TestHostInfoUsesTheCartridgeNameNotTheMountpointBasename(t *testing.T) {
	t.Setenv(config.ForceHostedImageEnvVar, "")
	t.Setenv(config.ForceDebianImageEnvVar, "")

	mount := "/Volumes/" + cartridge.VolumeName("demo")
	h, err := New(Spec{
		Kind:          instance.KindCartridge,
		CartridgePath: "/Users/someone/Downloads/demo.dmg",
		Mountpoint:    mount,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.AdoptCartridge(&cartridge.Opened{
		Name:        "demo",
		SourcePath:  "/Users/someone/Downloads/demo.dmg",
		WorkingCopy: "/Users/someone/Downloads/demo.sparseimage",
		Mount:       cartridge.Mount{Mountpoint: mount, DevNode: "/dev/disk9s1"},
	})
	// The config the Host resolves for a cartridge is rooted at the mountpoint,
	// which is exactly what used to supply the wrong name.
	h.cfg = &config.Config{VMDir: mount}

	if got := h.instanceName(); got != "demo" {
		t.Fatalf("instanceName() = %q, want %q", got, "demo")
	}
	e := h.Info()
	if e.Name != "demo" {
		t.Errorf("Info().Name = %q, want demo (the name the user typed)", e.Name)
	}
	if err := instance.ValidName(e.Name); err != nil {
		t.Errorf("published name is not registrable: %v", err)
	}
	if e.StateDir != mount || e.Mountpoint != mount {
		t.Errorf("StateDir = %q, Mountpoint = %q, want %q", e.StateDir, e.Mountpoint, mount)
	}
	if e.WorkingCopy != "/Users/someone/Downloads/demo.sparseimage" {
		t.Errorf("WorkingCopy = %q", e.WorkingCopy)
	}
}

// The name resolution order, and its fallbacks.
func TestHostInstanceNamePrecedence(t *testing.T) {
	t.Setenv(config.ForceHostedImageEnvVar, "")
	t.Setenv(config.ForceDebianImageEnvVar, "")

	withCartridge := func(spec Spec, name string, cfg *config.Config) *Host {
		t.Helper()
		h, err := New(spec)
		if err != nil {
			t.Fatal(err)
		}
		if name != "" {
			h.AdoptCartridge(&cartridge.Opened{Name: name, Mount: cartridge.Mount{Mountpoint: "/Volumes/x"}})
		}
		h.cfg = cfg
		return h
	}
	cartSpec := Spec{Kind: instance.KindCartridge, CartridgePath: "/tmp/demo.dmg", Mountpoint: "/Volumes/bladerunner-demo"}

	// An explicit Spec.Name always wins.
	named := cartSpec
	named.Name = "slot"
	if got := withCartridge(named, "demo", &config.Config{VMDir: "/Volumes/bladerunner-demo"}).instanceName(); got != "slot" {
		t.Errorf("explicit spec name = %q, want slot", got)
	}
	// An unusable cartridge name falls through to the derived one rather than
	// producing an entry the registry has to refuse.
	if got := withCartridge(cartSpec, "Demo Cartridge", &config.Config{VMDir: "/state/disks/fallback"}).instanceName(); got != "fallback" {
		t.Errorf("fallback name = %q, want fallback", got)
	}
	// No cartridge at all: the state dir basename, as before.
	flat, err := New(Spec{Kind: instance.KindDisk})
	if err != nil {
		t.Fatal(err)
	}
	flat.cfg = &config.Config{VMDir: "/state/disks/incus"}
	if got := flat.instanceName(); got != "incus" {
		t.Errorf("disk slot name = %q, want incus", got)
	}
	// Before the config is resolved there is simply no name yet.
	bare, err := New(Spec{Kind: instance.KindFlat})
	if err != nil {
		t.Fatal(err)
	}
	if got := bare.instanceName(); got != "" {
		t.Errorf("instanceName() before the config resolves = %q, want empty", got)
	}
}

// The veto is registered for the BSD NAME of the cartridge's device, which is
// the only thing DiskArbitration will match. Registering the device PATH — as
// this did — filtered every callback out, so the approval callback never ran,
// nothing was ever vetoed, and a Finder eject pulled the disk out from under a
// live VMM.
//
// The reduction itself is specified once, in internal/diskarb; what this
// asserts is the rule at the level it broke — the filter a Host computes from
// the device node it recorded MATCHES the name DiskArbitration will report.
func TestUnmountFilterMatchesWhatDiskArbitrationReports(t *testing.T) {
	t.Setenv(config.ForceHostedImageEnvVar, "")
	t.Setenv(config.ForceDebianImageEnvVar, "")

	reported := diskarb.DiskInfo{BSDName: "disk9s1"}

	// Every spelling a cartridge might have recorded its device under has to
	// arm a watcher that sees the slice the unmount request arrives for —
	// including the whole disk, which is all a caller who attached a DMG knows.
	for _, recorded := range []string{"/dev/disk9s1", "disk9s1", "/dev/disk9", "/dev/rdisk9s1"} {
		h := hostWithCartridgeDevNode(t, recorded)
		filter := h.unmountFilter()
		if filter == "" {
			t.Fatalf("a cartridge on %q armed no filter at all", recorded)
		}
		if !diskarb.MatchesFilter(filter, reported.BSDName) {
			t.Errorf("a cartridge recorded as %q registers filter %q, which does not match the reported %q",
				recorded, filter, reported.BSDName)
		}
		if strings.HasPrefix(filter, "/") {
			t.Errorf("filter %q is a path; DiskInfo.BSDName never is, so the veto would never fire", filter)
		}
	}

	// A device node that names no disk must arm NOTHING: an empty diskarb
	// filter matches every disk on the machine, which would veto unmounts that
	// are none of this instance's business.
	for _, recorded := range []string{"", "/Volumes/bladerunner-demo"} {
		if got := hostWithCartridgeDevNode(t, recorded).unmountFilter(); got != "" {
			t.Errorf("a cartridge recorded as %q armed filter %q, want none", recorded, got)
		}
	}

	// No cartridge at all: nothing to watch, and no panic.
	h, err := New(Spec{Kind: instance.KindCartridge, Name: "demo", CartridgePath: "/tmp/demo.dmg", Mountpoint: "/Volumes/bladerunner-demo"})
	if err != nil {
		t.Fatal(err)
	}
	if got := h.unmountFilter(); got != "" {
		t.Fatalf("unmountFilter() with no cartridge = %q, want empty", got)
	}
}

// hostWithCartridgeDevNode builds a cartridge Host that has adopted a mount
// whose device node is recorded as devNode.
func hostWithCartridgeDevNode(t *testing.T, devNode string) *Host {
	t.Helper()
	h, err := New(Spec{
		Kind:          instance.KindCartridge,
		Name:          "demo",
		CartridgePath: "/tmp/demo.dmg",
		Mountpoint:    "/Volumes/bladerunner-demo",
	})
	if err != nil {
		t.Fatal(err)
	}
	h.AdoptCartridge(&cartridge.Opened{
		Name:  "demo",
		Mount: cartridge.Mount{Mountpoint: "/Volumes/bladerunner-demo", DevNode: devNode},
	})
	return h
}

// Drain before the VM exists must say so rather than panic.
func TestDrainBeforeStart(t *testing.T) {
	t.Setenv(config.ForceHostedImageEnvVar, "")
	t.Setenv(config.ForceDebianImageEnvVar, "")

	h, err := New(Spec{Kind: instance.KindFlat})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Drain(context.Background(), time.Second); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("Drain() = %v, want %v", err, ErrNotStarted)
	}
	if h.Runner() != nil {
		t.Error("Runner() should be nil before start")
	}
	// stop() before Run must be a harmless no-op.
	h.stop()
}

// A nil Observer restores the silent default rather than panicking later.
func TestSetObserverNilIsSilentDefault(t *testing.T) {
	t.Setenv(config.ForceHostedImageEnvVar, "")
	t.Setenv(config.ForceDebianImageEnvVar, "")

	h, err := New(Spec{Kind: instance.KindFlat})
	if err != nil {
		t.Fatal(err)
	}
	h.SetObserver(nil)
	h.obs.Resolved(nil)
	h.obs.Waiting(false)
	h.obs.Stopping()
	if p := h.obs.Progress(context.Background(), nil); p != nil {
		t.Fatalf("NopObserver.Progress = %v, want nil", p)
	}
}

// AdoptCartridge(nil) must be a no-op so a caller can hand over whatever it has
// without branching.
func TestAdoptCartridgeNil(t *testing.T) {
	t.Setenv(config.ForceHostedImageEnvVar, "")
	t.Setenv(config.ForceDebianImageEnvVar, "")

	h, err := New(Spec{Kind: instance.KindFlat})
	if err != nil {
		t.Fatal(err)
	}
	h.AdoptCartridge(nil)
	if h.Cartridge() != nil || h.adopted {
		t.Fatal("AdoptCartridge(nil) must not adopt anything")
	}
	// The cartridge step is a no-op for a non-cartridge instance.
	if err := h.startCartridge(); err != nil {
		t.Fatalf("startCartridge on a flat instance = %v", err)
	}
	if err := h.stopCartridge(); err != nil {
		t.Fatalf("stopCartridge with no cartridge = %v", err)
	}
}

func TestEjectTimeoutFromArgs(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want time.Duration
	}{
		{"explicit seconds", "eject 45", 45 * time.Second},
		{"explicit with force", "eject 30 force", 30 * time.Second},
		{"absent uses default", "eject", control.DefaultEjectTimeoutSeconds * time.Second},
		{"zero uses default", "eject 0", control.DefaultEjectTimeoutSeconds * time.Second},
		{"garbage uses default", "eject abc", control.DefaultEjectTimeoutSeconds * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := control.NewRequest(tc.raw)
			if got := ejectTimeoutFromArgs(req); got != tc.want {
				t.Fatalf("ejectTimeoutFromArgs(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestEjectForceFromArgs(t *testing.T) {
	if !ejectForceFromArgs(control.NewRequest("eject 30 force")) {
		t.Error("force arg should be detected")
	}
	if ejectForceFromArgs(control.NewRequest("eject 30")) {
		t.Error("no force arg should be false")
	}
	if ejectForceFromArgs(control.NewRequest("eject")) {
		t.Error("absent args should be false")
	}
}
