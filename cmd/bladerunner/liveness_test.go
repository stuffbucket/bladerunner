package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/control"
	"github.com/stuffbucket/bladerunner/internal/instance"
)

// noInstanceSelected clears every way of naming an instance, so a test exercises
// the IMPLICIT resolution path — the one a user reaches by typing `br stop`
// with no --instance, which is what the "use 'br stop --force'" advice tells
// them to type.
func noInstanceSelected(t *testing.T) {
	t.Helper()
	saved := instanceFlag
	instanceFlag = ""
	t.Cleanup(func() { instanceFlag = saved })
	t.Setenv(instanceEnvVar, "")
	t.Setenv(instanceEnvVarAlias, "")
}

// registerWedgedNamedInstance wedges a NAMED instance under a root that holds
// nothing else, and returns it. The flat default at the root is left empty on
// purpose: it is what the resolver used to fall back to.
func registerWedgedNamedInstance(t *testing.T, name string) (root string, h wedgedHolder) {
	t.Helper()
	root = shortStateDir(t)
	t.Setenv("BLADERUNNER_STATE_DIR", root)
	noInstanceSelected(t)

	h = wedgeHolderAt(t, filepath.Join(root, name))
	register(t, root, instance.Entry{
		Name: name, Kind: instance.KindDisk, StateDir: h.stateDir, PID: h.pid,
	})
	return root, h
}

// TestResolveFindsAWedgedNamedInstance is the regression test for issue #290.
//
// Instance resolution filtered its candidates with control.Client.IsRunning,
// which is a ping round trip. A holder that is alive but wedged answers no
// ping, so no candidate survived the filter, resolution fell through to the
// flat default — with PID 0 — and never read the registry entry that knew
// where the wedged instance was. The bare `br stop --force` that every
// unresponsive-VM message suggests therefore acted on the wrong instance, and
// the wedged one could only be reached by a user who already knew its name and
// typed --instance.
func TestResolveFindsAWedgedNamedInstance(t *testing.T) {
	root, h := registerWedgedNamedInstance(t, "demo")

	if control.NewClient(h.stateDir).IsRunning() {
		t.Fatal("the stand-in holder answered a ping; it is not wedged")
	}

	target, err := resolveInstanceTarget()
	if err != nil {
		t.Fatalf("resolveInstanceTarget: %v", err)
	}
	if target.Name != "demo" {
		t.Errorf("resolved %q at %q, want the wedged instance \"demo\" at %q",
			target.Name, target.StateDir, h.stateDir)
	}
	if target.StateDir != h.stateDir {
		t.Errorf("StateDir = %q, want %q (the flat default %q is empty)",
			target.StateDir, h.stateDir, root)
	}
	if target.Fallback {
		t.Error("resolved to the flat default fallback while a wedged instance was registered")
	}
	if !target.isLive() {
		t.Errorf("Liveness = %s, want a live rung", target.Liveness)
	}
	if got := holderPID(target); got != h.pid {
		t.Errorf("holderPID = %d, want the wedged holder %d", got, h.pid)
	}
}

// TestStopForceRecoversAWedgedNamedInstance is the other half of #290: the
// remedy the error message names has to work as typed. `br stop --force` with
// no --instance must terminate the wedged NAMED holder.
func TestStopForceRecoversAWedgedNamedInstance(t *testing.T) {
	_, h := registerWedgedNamedInstance(t, "demo")
	useStopFlags(t, true)

	if err := runStop(nil, nil); err != nil {
		t.Fatalf("runStop --force = %v, want nil (it must reach the wedged named holder)", err)
	}
	if instance.ProcessAlive(h.pid) {
		t.Errorf("holder process %d is still alive after 'br stop --force'", h.pid)
	}
	if _, err := os.Stat(h.socketPath); !os.IsNotExist(err) {
		t.Errorf("stale control socket %s was left behind", h.socketPath)
	}
}

// TestStopReportsAWedgedNamedInstanceWithoutForce holds that the report names
// the wedged instance's own holder, so the user can see what --force will
// terminate before they type it.
func TestStopReportsAWedgedNamedInstanceWithoutForce(t *testing.T) {
	_, h := registerWedgedNamedInstance(t, "demo")
	useStopFlags(t, false)

	err := runStop(nil, nil)
	if err == nil {
		t.Fatal("runStop on a wedged named instance = nil, want an error describing the wedge")
	}
	if strings.Contains(err.Error(), "VM is not running") {
		t.Errorf("error = %q; the holder is alive, so this report is false", err)
	}
	if !strings.Contains(err.Error(), h.socketPath) {
		t.Errorf("error = %q, want it to name the wedged instance's socket %q", err, h.socketPath)
	}
	if !instance.ProcessAlive(h.pid) {
		t.Errorf("holder process %d was terminated without --force", h.pid)
	}
}

// TestResolveNeverSignalsARecycledPID is the guard on the fix.
//
// Resolution now surfaces the ProcessOnly rung, which rests on a recorded PID.
// A holder killed with SIGKILL leaves its socket inode and its registry entry
// behind, so once the OS reuses its PID that entry names an innocent process.
// Resolving to it is harmless and even useful — the entry is what says the
// instance is stale — but signaling it is not, so `br stop --force` must
// still refuse. The discriminator is the connect, which a leftover inode
// refuses with ECONNREFUSED.
func TestResolveNeverSignalsARecycledPID(t *testing.T) {
	root := shortStateDir(t)
	t.Setenv("BLADERUNNER_STATE_DIR", root)
	noInstanceSelected(t)
	useStopFlags(t, true)

	stateDir := filepath.Join(root, "demo")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	// A crashed holder's leavings: a socket FILE with nothing listening.
	if err := os.WriteFile(control.SocketPath(stateDir), nil, 0o600); err != nil {
		t.Fatalf("write stale socket: %v", err)
	}
	innocent := startStandInHolder(t)
	register(t, root, instance.Entry{
		Name: "demo", Kind: instance.KindDisk, StateDir: stateDir, PID: innocent,
	})

	err := runStop(nil, nil)
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("runStop --force = %v, want it to report the instance is not running", err)
	}
	if !instance.ProcessAlive(innocent) {
		t.Errorf("br stop --force terminated pid %d, which is not a bladerunner holder", innocent)
	}
}

// TestInstanceHeldSeparatesAWedgeFromLitter holds the contract every
// "do not start a second one over this" guard depends on: a wedged holder is
// held, and the socket file a dead one left behind is not.
func TestInstanceHeldSeparatesAWedgeFromLitter(t *testing.T) {
	h := wedgeHolderAt(t, shortStateDir(t))
	if !instanceHeld(h.stateDir) {
		t.Error("instanceHeld on a wedged holder = false; its socket is bound and its process is alive")
	}
	if got := livenessAt(h.stateDir); got != instance.Serving {
		t.Errorf("livenessAt on a wedged holder = %s, want %s (the socket accepts, it just never replies)", got, instance.Serving)
	}
	if err := alreadyRunningAt(h.stateDir); err == nil {
		t.Error("alreadyRunningAt on a wedged holder = nil; a second holder would collide with its start lock")
	}

	dead := shortStateDir(t)
	if err := os.WriteFile(control.SocketPath(dead), nil, 0o600); err != nil {
		t.Fatalf("write stale socket: %v", err)
	}
	exited := exec.Command("/bin/sh", "-c", "exit 0")
	if err := exited.Run(); err != nil {
		t.Fatalf("run short-lived process: %v", err)
	}
	if err := os.WriteFile(control.LockPath(dead), fmt.Appendf(nil, "%d\n", exited.Process.Pid), 0o600); err != nil {
		t.Fatalf("write control lock: %v", err)
	}
	// The socket FILE is present, which is exactly the signal the ladder
	// refuses to trust: only the connect tells a bound socket from an inode.
	if got := livenessAt(dead); got != instance.Dead {
		t.Errorf("livenessAt on a dead holder's litter = %s, want %s", got, instance.Dead)
	}
	if err := alreadyRunningAt(dead); err != nil {
		t.Errorf("alreadyRunningAt on a dead holder's litter = %v, want nil", err)
	}
}

// TestUnresponsiveErrorNamesTheHolderAndTheRemedy holds the wording contract
// the reporting sites share: the user has to learn that something IS there,
// which one, and what terminates it.
func TestUnresponsiveErrorNamesTheHolderAndTheRemedy(t *testing.T) {
	dir := shortStateDir(t)
	if err := os.WriteFile(control.LockPath(dir), []byte("4242\n"), 0o600); err != nil {
		t.Fatalf("write control lock: %v", err)
	}
	got := unresponsiveError("the default VM", dir).Error()
	for _, want := range []string{"the default VM", "unresponsive", "4242", control.SocketPath(dir), "br stop --force"} {
		if !strings.Contains(got, want) {
			t.Errorf("unresponsiveError = %q, want it to contain %q", got, want)
		}
	}

	// With no lock there is no PID to name, and the message must not invent one.
	if err := os.Remove(control.LockPath(dir)); err != nil {
		t.Fatalf("remove control lock: %v", err)
	}
	got = unresponsiveError("the default VM", dir).Error()
	if strings.Contains(got, "process 0") {
		t.Errorf("unresponsiveError = %q, want no PID when none is recorded", got)
	}
	if !strings.Contains(got, "br stop --force") {
		t.Errorf("unresponsiveError = %q, want the remedy even without a PID", got)
	}
}

// TestRequireRunningVMReportsAWedgeRatherThanAbsence holds that a verb needing
// a VM does not tell the user their instance is stopped when it is wedged, and
// does not offer to start a second one over it.
func TestRequireRunningVMReportsAWedgeRatherThanAbsence(t *testing.T) {
	h := startWedgedHolder(t)
	target := resolvedInstance{
		Name: config.DefaultInstanceName, Kind: instance.KindFlat, StateDir: h.stateDir,
	}

	client, err := requireRunningVM(target)
	if err == nil {
		t.Fatalf("requireRunningVM on a wedged holder = %v, want an error", client)
	}
	if !strings.Contains(err.Error(), "br stop --force") {
		t.Errorf("error = %q, want it to name 'br stop --force'", err)
	}
	if !instance.ProcessAlive(h.pid) {
		t.Errorf("holder process %d was terminated by a read-only gate", h.pid)
	}
}
