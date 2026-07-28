package main

// Which process runs the VM.
//
// Goal 1 of the cartridge runtime is that the VM is owned by a minimal wrapper
// process, so the CLI can come and go without killing it. These tests pin the
// decision that makes it true on the ORDINARY paths — `br start`, `br up`,
// `br boot` — and the one exception that keeps a GUI boot working.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/cartridge"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/control"
	"github.com/stuffbucket/bladerunner/internal/instance"
	"github.com/stuffbucket/bladerunner/internal/vmhost"
)

// A headless start must NOT run the VM in this process. Every spec below is one
// the ordinary paths build, and every one of them has to route to a holder.
func TestHeadlessStartsRunUnderAHolder(t *testing.T) {
	t.Setenv("BLADERUNNER_STATE_DIR", t.TempDir())

	cases := []struct {
		name string
		spec vmhost.Spec
	}{
		{
			name: "plain br start",
			spec: vmhost.Spec{Kind: instance.KindFlat},
		},
		{
			name: "br start with flags but no --gui",
			spec: vmhost.Spec{
				Kind:         instance.KindFlat,
				Overrides:    vmhost.Overrides{CPUs: 8, GUI: true},
				ChangedFlags: []string{"cpus"},
			},
		},
		{
			name: "br boot <disk> --headless",
			spec: vmhost.Spec{
				Kind:      instance.KindDisk,
				Driven:    true,
				Overrides: vmhost.Overrides{CPUs: 2, MemoryGiB: 4, DiskSizeGiB: 32, GUI: false},
			},
		},
		{
			name: "br boot <cartridge>",
			spec: vmhost.Spec{Kind: instance.KindCartridge, Name: "demo", CartridgePath: "/img/demo.dmg"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if guiRequested(tc.spec) {
				t.Fatal("routed to the foreground; a headless start must run under a holder")
			}
		})
	}
}

// A GUI boot stays in the foreground, because vz.StartGraphicApplication owns
// the main thread of whichever process calls it and the window has to belong to
// the one the user typed in.
func TestGUIStartsStayInTheForeground(t *testing.T) {
	t.Setenv("BLADERUNNER_STATE_DIR", t.TempDir())

	explicit := vmhost.Spec{
		Kind:         instance.KindFlat,
		Overrides:    vmhost.Overrides{GUI: true},
		ChangedFlags: []string{"gui"},
	}
	if !guiRequested(explicit) {
		t.Error("br start --gui must stay in the foreground")
	}

	driven := vmhost.Spec{
		Kind:      instance.KindDisk,
		Driven:    true,
		Overrides: vmhost.Overrides{CPUs: 2, MemoryGiB: 4, DiskSizeGiB: 32, GUI: true},
	}
	if !guiRequested(driven) {
		t.Error("br boot --gui must stay in the foreground")
	}
}

// The menubar's "show console" switch is a persisted Setting, not a flag, and a
// start that inherits it opens a window just as surely as --gui does. It has to
// reach the same decision, or the window would open on a detached holder.
func TestPersistedShowConsoleKeepsTheForeground(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("BLADERUNNER_STATE_DIR", stateDir)

	s := config.DefaultSettings()
	s.ShowConsole = true
	if err := s.Save(stateDir); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	if !guiRequested(vmhost.Spec{Kind: instance.KindFlat}) {
		t.Error("a start that inherits ShowConsole must stay in the foreground")
	}

	// A DRIVEN spec carries its own pre-resolved answer and must not consult
	// the setting: `br boot <disk> --headless` means headless.
	driven := vmhost.Spec{
		Kind:      instance.KindDisk,
		Driven:    true,
		Overrides: vmhost.Overrides{CPUs: 2, MemoryGiB: 4, DiskSizeGiB: 32, GUI: false},
	}
	if guiRequested(driven) {
		t.Error("an explicit --headless boot must not be overridden by ShowConsole")
	}
}

// bootCmdForTest returns a `br boot` command with the given flags marked as
// changed, standing in for what cobra does on a real invocation.
func bootCmdForTest(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "boot", RunE: func(*cobra.Command, []string) error { return nil }}
	f := cmd.Flags()
	f.UintVar(&bootFlags.cpus, "cpus", 0, "")
	f.Uint64Var(&bootFlags.memory, "memory", 0, "")
	f.IntVar(&bootFlags.disk, "disk", 0, "")
	f.BoolVar(&bootFlags.headless, "headless", false, "")
	f.DurationVar(&bootFlags.timeout, "timeout", config.DefaultTimeout, "")
	if err := f.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return cmd
}

// A cartridge boot cannot resolve its own sizing: the recommendations live in a
// manifest inside an image nothing has attached yet. So the spec must report
// ONLY the flags the user set, leaving the cartridge's manifest and the
// persisted Settings their say over the rest.
func TestBootOverridesReportOnlyChangedFlags(t *testing.T) {
	saved := bootFlags
	t.Cleanup(func() { bootFlags = saved })

	cmd := bootCmdForTest(t, "--cpus", "6", "--headless")
	o, changed := bootOverrides(cmd)

	if o.CPUs != 6 {
		t.Errorf("CPUs = %d, want 6", o.CPUs)
	}
	if got, want := strings.Join(changed, ","), "cpus,gui"; got != want {
		t.Errorf("changed flags = %q, want %q", got, want)
	}
	if o.GUI {
		t.Error("--headless must assert GUI off")
	}
}

// Nothing set means nothing asserted, so a cartridge boots at the size it was
// packed for.
func TestBootOverridesAssertNothingByDefault(t *testing.T) {
	saved := bootFlags
	t.Cleanup(func() { bootFlags = saved })

	_, changed := bootOverrides(bootCmdForTest(t))
	if len(changed) != 0 {
		t.Errorf("changed flags = %v, want none", changed)
	}
}

// `br boot <dmg> --persist --private-mount` has to reach the holder, which is
// what opens the cartridge now. Both options used to live only on the
// already-open *cartridge.Opened, so a holder-run boot dropped them silently —
// and dropping --persist means discarding the guest's changes while the user
// was told they would be kept.
func TestCartridgeBootSpecCarriesThePersistenceOptions(t *testing.T) {
	saved := bootFlags
	t.Cleanup(func() { bootFlags = saved })
	t.Setenv("BLADERUNNER_STATE_DIR", t.TempDir())

	cmd := bootCmdForTest(t)
	bootFlags.persist = true
	bootFlags.privateMount = true

	spec := cartridgeBootSpec(cmd, "/img/demo.dmg", "demo")
	if !spec.Persist {
		t.Error("--persist did not reach the spec")
	}
	if !spec.MountPolicy.Private() {
		t.Errorf("MountPolicy = %q, want private", spec.MountPolicy)
	}
	if spec.Mountpoint == "" || filepath.Base(spec.Mountpoint) != "demo" {
		t.Errorf("Mountpoint = %q, want the private slot for demo", spec.Mountpoint)
	}
	if err := spec.Validate(); err != nil {
		t.Fatalf("the spec a cartridge boot builds must be runnable: %v", err)
	}
}

// Under the browsable default the CLI dictates no mountpoint: macOS chooses
// one, and the holder reads it back.
func TestCartridgeBootSpecLeavesTheBrowsableMountpointToMacOS(t *testing.T) {
	saved := bootFlags
	t.Cleanup(func() { bootFlags = saved })
	t.Setenv("BLADERUNNER_STATE_DIR", t.TempDir())

	bootFlags.persist = false
	bootFlags.privateMount = false
	spec := cartridgeBootSpec(bootCmdForTest(t), "/img/demo.dmg", "demo")

	if spec.MountPolicy != cartridge.MountBrowsable {
		t.Errorf("MountPolicy = %q, want browsable", spec.MountPolicy)
	}
	if spec.Mountpoint != "" {
		t.Errorf("Mountpoint = %q, want none", spec.Mountpoint)
	}
	if spec.Driven {
		t.Error("a cartridge spec must not be driven: its sizing is inside the image")
	}
	if err := spec.Validate(); err != nil {
		t.Fatalf("the spec a cartridge boot builds must be runnable: %v", err)
	}
}

// The "already up" refusal has to be made by the CLI now. The Host still makes
// it, but it makes it inside a detached process whose only output is a log
// file, so a terminal that relied on it would print a pid and then wait for a
// readiness that could never arrive.
func TestAlreadyRunningAtRefusesALiveInstance(t *testing.T) {
	// A short path: a unix socket address has a hard length limit that
	// t.TempDir() exceeds on macOS.
	dir, err := os.MkdirTemp("", "brsock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	srv, err := control.NewServer(dir, func() {})
	if err != nil {
		t.Fatalf("stand up a control listener: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go srv.Start(ctx)
	t.Cleanup(func() {
		cancel()
		_ = srv.Close()
	})

	err = alreadyRunningAt(dir)
	if err == nil {
		t.Fatal("alreadyRunningAt accepted a live instance")
	}
	if !strings.Contains(err.Error(), "br stop") {
		t.Errorf("refusal = %v, want it to name the way out", err)
	}
}

// Nothing listening is not an error: that is the ordinary case.
func TestAlreadyRunningAtAcceptsAnIdleInstance(t *testing.T) {
	if err := alreadyRunningAt(t.TempDir()); err != nil {
		t.Fatalf("alreadyRunningAt(idle) = %v, want nil", err)
	}
}

// A holder that exited during startup must produce an error naming what it
// wrote, because a detached process has no terminal and its log is the only
// place its reason exists.
func TestHolderDiedQuotesTheLog(t *testing.T) {
	dir := t.TempDir()
	logPath := vmdLogPath(dir, "")
	if err := os.WriteFile(logPath, []byte("line one\nboot failed: no such image\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &holderAttachment{
		spec:      vmhost.Spec{Kind: instance.KindFlat, StateDir: dir},
		pid:       424242,
		spawnedAt: time.Now(),
	}
	err := a.holderDied()
	if err == nil {
		t.Fatal("holderDied returned no error")
	}
	if !strings.Contains(err.Error(), "boot failed: no such image") {
		t.Errorf("error = %v, want it to quote the holder log", err)
	}
	if !strings.Contains(err.Error(), logPath) {
		t.Errorf("error = %v, want it to name %q", err, logPath)
	}
}

// tailFile is best effort: it enriches a failure and must never produce one.
func TestTailFileOnAMissingFile(t *testing.T) {
	if got := tailFile(filepath.Join(t.TempDir(), "absent.log"), 5); got != "" {
		t.Errorf("tailFile(missing) = %q, want empty", got)
	}
}

// The stop hint has to name the instance, because `br stop` alone addresses the
// default one and would leave a booted cartridge running.
func TestStopCommandNamesTheInstance(t *testing.T) {
	cart := &holderAttachment{spec: vmhost.Spec{Kind: instance.KindCartridge, Name: "demo"}}
	if got, want := cart.stopCommand(), "br stop --instance demo"; got != want {
		t.Errorf("stopCommand() = %q, want %q", got, want)
	}
	flat := &holderAttachment{spec: vmhost.Spec{Kind: instance.KindFlat}}
	if got, want := flat.stopCommand(), "br stop"; got != want {
		t.Errorf("stopCommand() = %q, want %q", got, want)
	}
}

// There is ONE instance registry per machine and it lives beside the default
// instance, so a lookup keyed on the instance's own state directory finds
// nothing for a disk slot or a custom --state-dir. This pins that the
// attachment reads the registry where the holder actually writes it.
func TestAttachmentReadsTheOneRegistry(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BLADERUNNER_STATE_DIR", root)

	slot := filepath.Join(root, "disks", "demo")
	if err := os.MkdirAll(slot, 0o755); err != nil {
		t.Fatal(err)
	}
	want := instance.Entry{
		Name:     "demo",
		Kind:     instance.KindDisk,
		StateDir: slot,
		PID:      os.Getpid(),
		Ports:    instance.Ports{SSH: 61234, API: 61235},
	}
	if err := instance.Write(vmhost.RegistryRoot(), want); err != nil {
		t.Fatalf("publish the entry where a holder would: %v", err)
	}

	a := &holderAttachment{spec: vmhost.Spec{Kind: instance.KindDisk, StateDir: slot}}
	if got := a.instanceNameFor(slot); got != "demo" {
		t.Fatalf("instanceNameFor(%q) = %q, want demo", slot, got)
	}
	got, err := instance.Read(vmhost.RegistryRoot(), a.instanceNameFor(slot))
	if err != nil {
		t.Fatalf("the attachment cannot find the entry a holder published: %v", err)
	}
	if got.Ports.SSH != want.Ports.SSH {
		t.Errorf("Ports.SSH = %d, want %d", got.Ports.SSH, want.Ports.SSH)
	}
	// And the slot itself must NOT hold a registry of its own.
	if _, err := os.Stat(filepath.Join(slot, "instances")); err == nil {
		t.Error("a disk slot has its own instances/ directory; the lookup key is ambiguous")
	}
}

// Ctrl+C must END the wait, not merely be noticed by it.
//
// The detach is not a failure — the VM is deliberately left running — so it is
// tempting to report it as nil. Doing that inside the poll loop makes the loop
// treat it as "nothing happened yet" and spin forever on a terminal the user
// has already walked away from; the sentinel is what unwinds it, and only the
// top of the call chain turns it back into success.
func TestPauseEndsTheWaitOnDetach(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	a := &holderAttachment{
		spec:      vmhost.Spec{Kind: instance.KindFlat, StateDir: t.TempDir()},
		pid:       os.Getpid(),
		opts:      holderAttachOptions{Quiet: true},
		spawnedAt: time.Now(),
	}
	if err := a.pause(ctx); !errors.Is(err, errHolderDetached) {
		t.Fatalf("pause(canceled) = %v, want errHolderDetached", err)
	}

	// And the wait loops must unwind rather than spin. A canceled context makes
	// each of them return immediately.
	done := make(chan error, 1)
	go func() {
		_, err := a.awaitTerminalStage(ctx, a.spec.StateDir)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, errHolderDetached) {
			t.Fatalf("awaitTerminalStage(canceled) = %v, want errHolderDetached", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("awaitTerminalStage did not unwind on a canceled context")
	}
}

// A detach is not a command failure: the VM is running, which is what was
// asked for. startUnderHolder's caller must see success.
func TestDetachIsNotAnError(t *testing.T) {
	a := &holderAttachment{opts: holderAttachOptions{Quiet: true}}
	if err := a.finish(errHolderDetached); err != nil {
		t.Fatalf("finish(errHolderDetached) = %v, want nil", err)
	}
	other := errors.New("holder exploded")
	if err := a.finish(other); !errors.Is(err, other) {
		t.Fatalf("finish(%v) = %v, want it passed through", other, err)
	}
}
