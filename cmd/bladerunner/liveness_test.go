package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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

// TestEjectSelectionAgreesWithResolve holds that the implicit-selection policy
// has ONE implementation.
//
// `br eject` with no name re-derived the candidate set from liveInstances() and
// switched on its length itself, bypassing the strongest-rung filter. A
// ProcessOnly phantom — a crashed holder's entry whose PID the OS reused — then
// made `br eject` fail as ambiguous while `br stop` resolved cleanly: one
// policy, two implementations, disagreeing (AGENTS.md section 3).
func TestEjectSelectionAgreesWithResolve(t *testing.T) {
	root := t.TempDir()
	alpha := filepath.Join(root, "alpha")
	beta := filepath.Join(root, "beta")
	register(t, root, instance.Entry{Name: "alpha", Kind: instance.KindDisk, StateDir: alpha})
	register(t, root, instance.Entry{Name: "beta", Kind: instance.KindDisk, StateDir: beta})

	scanner := testScannerWithRungs(root, map[string]instance.Liveness{
		filepath.Clean(alpha): instance.Serving,
		filepath.Clean(beta):  instance.ProcessOnly,
	})

	resolved, err := scanner.resolve("")
	if err != nil {
		t.Fatalf("resolve(\"\"): %v", err)
	}
	baseDir, slot, err := scanner.ejectSlot("")
	if err != nil {
		t.Fatalf("ejectSlot(\"\") = %v, but resolve(\"\") picked %q; the two must agree", err, resolved.Name)
	}
	if baseDir != resolved.StateDir {
		t.Errorf("ejectSlot picked %q, resolve picked %q", baseDir, resolved.StateDir)
	}
	if slot != "alpha" {
		t.Errorf("ejectSlot named %q, want %q", slot, "alpha")
	}
}

// TestEjectDoesNotCallAWedgedHolderNotBooted holds that the ladder and the ping
// do not disagree inside one command. `br eject` resolved a wedged holder
// through the ladder and then gated on a ping three lines later, so it found
// the instance and immediately reported it as not booted.
func TestEjectDoesNotCallAWedgedHolderNotBooted(t *testing.T) {
	h := startWedgedHolder(t)
	noInstanceSelected(t)
	saved := ejectFlags
	t.Cleanup(func() { ejectFlags = saved })
	ejectFlags.disk = ""
	ejectFlags.timeout = time.Second

	err := runEject(nil, nil)
	if err == nil {
		t.Fatal("runEject on a wedged holder = nil, want an error describing the wedge")
	}
	if strings.Contains(err.Error(), "is not booted") {
		t.Errorf("runEject reported %q; the holder is alive and its socket is bound, so this is false", err)
	}
	if !strings.Contains(err.Error(), "br stop --force") {
		t.Errorf("runEject reported %q, want it to name the remedy for a wedged holder", err)
	}
	if !instance.ProcessAlive(h.pid) {
		t.Errorf("holder process %d was terminated by br eject", h.pid)
	}
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
	_, innocent := registerRecycledPIDInstance(t, "demo")
	useStopFlags(t, true)

	err := runStop(nil, nil)
	if err == nil {
		t.Fatal("runStop --force = nil, want an error")
	}
	if !instance.ProcessAlive(innocent) {
		t.Errorf("br stop --force terminated pid %d, which is not a bladerunner holder", innocent)
	}
}

// registerRecycledPIDInstance builds the state a SIGKILLed holder leaves behind
// once the OS has reused its PID: a control socket FILE with nothing listening,
// a start lock and a registry entry both naming a live but unrelated process.
// It returns the instance's state dir and that innocent PID.
func registerRecycledPIDInstance(t *testing.T, name string) (stateDir string, innocent int) {
	t.Helper()
	root := shortStateDir(t)
	t.Setenv("BLADERUNNER_STATE_DIR", root)
	noInstanceSelected(t)

	stateDir = filepath.Join(root, name)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	if err := os.WriteFile(control.SocketPath(stateDir), nil, 0o600); err != nil {
		t.Fatalf("write stale socket: %v", err)
	}
	innocent = startStandInHolder(t)
	if err := os.WriteFile(control.LockPath(stateDir), fmt.Appendf(nil, "%d\n", innocent), 0o600); err != nil {
		t.Fatalf("write control lock: %v", err)
	}
	register(t, root, instance.Entry{
		Name: name, Kind: instance.KindDisk, StateDir: stateDir, PID: innocent,
	})
	return stateDir, innocent
}

// TestHeldWithoutAListenerGivesAdviceThatWorks is the regression test for the
// dead end the adversarial review found.
//
// On the ProcessOnly rung — a leftover socket FILE with no listener plus a live
// PID in the lock, which is what a SIGKILLed or rebooted holder leaves once its
// PID is reused — every guard refuses, so `br up`, `br boot`, `br restore` and
// `br reset` all report the instance as held. They used to point at
// `br stop --force`, which answers "VM is not running" and does nothing,
// because --force deliberately refuses to signal a PID it cannot corroborate
// with a connect. The advice named no way out at all, and the escape that does
// work — removing the start lock — appeared nowhere.
func TestHeldWithoutAListenerGivesAdviceThatWorks(t *testing.T) {
	stateDir, innocent := registerRecycledPIDInstance(t, "demo")
	lock := control.LockPath(stateDir)

	if got := livenessAt(stateDir); got != instance.ProcessOnly {
		t.Fatalf("livenessAt = %s, want %s; the fixture is not on the rung under test", got, instance.ProcessOnly)
	}

	useStopFlags(t, true)
	stopErr := runStop(nil, nil)
	if stopErr == nil {
		t.Fatal("runStop --force = nil, want an error naming the lock")
	}
	target, err := resolveInstanceTarget()
	if err != nil {
		t.Fatalf("resolveInstanceTarget: %v", err)
	}
	_, gateErr := requireRunningVM(target)
	if gateErr == nil {
		t.Fatal("requireRunningVM = nil, want an error")
	}

	for _, tc := range []struct {
		site string
		err  error
	}{
		{"br stop --force", stopErr},
		{"the verb gate", gateErr},
	} {
		got := tc.err.Error()
		if !strings.Contains(got, lock) {
			t.Errorf("%s reported %q, want it to name the start lock %q — the only thing that clears this state", tc.site, got, lock)
		}
		if strings.Contains(got, "br stop --force") {
			t.Errorf("%s reported %q, but 'br stop --force' refuses to signal an uncorroborated PID; the advice cannot be followed", tc.site, got)
		}
		if !strings.Contains(got, strconv.Itoa(innocent)) {
			t.Errorf("%s reported %q, want it to name the recorded holder %d", tc.site, got, innocent)
		}
	}
	if !instance.ProcessAlive(innocent) {
		t.Errorf("pid %d was signaled; it is not a bladerunner holder", innocent)
	}

	// The guards that refuse to start a second holder point at `br stop` rather
	// than at --force, so their advice is followable through one hop — but only
	// because `br stop` now answers this rung truthfully. Assert the whole loop,
	// not just its last link: this is the chain that used to close on itself.
	refusal := alreadyRunningAt(stateDir)
	if refusal == nil {
		t.Fatal("alreadyRunningAt = nil on a held instance; a second holder would collide with the lock")
	}
	if !strings.Contains(refusal.Error(), "br stop") {
		t.Errorf("alreadyRunningAt = %q, want it to name a verb that reaches the state", refusal)
	}
	if strings.Contains(refusal.Error(), "br stop --force") {
		t.Errorf("alreadyRunningAt = %q, but --force refuses to act on this rung", refusal)
	}
}

// TestRemovingTheStartLockClearsAHeldInstance holds the claim the ProcessOnly
// advice makes about internal/control: removing the start lock is enough, and
// the leftover socket FILE beside it does not have to be removed by hand.
//
// The claim is about another component (AGENTS.md section 5, point 7), so it
// needs a test rather than a comment: NewListener unlinks a stale socket itself,
// but only after it wins the lock — so the lock is the whole obstruction.
func TestRemovingTheStartLockClearsAHeldInstance(t *testing.T) {
	stateDir, _ := registerRecycledPIDInstance(t, "demo")

	if _, err := control.NewListener(stateDir, control.NewLocalController(func() {})); err == nil {
		t.Fatal("NewListener succeeded while a live PID held the start lock")
	}
	if err := os.Remove(control.LockPath(stateDir)); err != nil {
		t.Fatalf("remove start lock: %v", err)
	}

	ln, err := control.NewListener(stateDir, control.NewLocalController(func() {}))
	if err != nil {
		t.Fatalf("NewListener after removing the lock = %v; the advice does not work", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	if livenessAt(stateDir) != instance.Serving {
		t.Error("the instance is not serving after a fresh listener bound its socket")
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

// TestHeldErrorGivesTheRemedyForItsRung holds the wording contract every
// reporting site shares. The user has to learn that something IS there, which
// process it is, and — critically — a remedy that actually applies to the rung
// they are on. Promising `br stop --force` where --force refuses to act is
// worse than saying nothing, because it reads as a way out and is not one.
func TestHeldErrorGivesTheRemedyForItsRung(t *testing.T) {
	dir := shortStateDir(t)
	live := startStandInHolder(t)

	t.Run("serving with a live holder names --force", func(t *testing.T) {
		got := heldError("the default VM", dir, instance.Serving, live).Error()
		for _, want := range []string{"the default VM", "unresponsive", strconv.Itoa(live), control.SocketPath(dir), "br stop --force"} {
			if !strings.Contains(got, want) {
				t.Errorf("heldError = %q, want it to contain %q", got, want)
			}
		}
	})

	t.Run("process-only names the start lock and not --force", func(t *testing.T) {
		got := heldError("the default VM", dir, instance.ProcessOnly, live).Error()
		if strings.Contains(got, "br stop --force") {
			t.Errorf("heldError = %q; --force refuses to signal a PID with no listener behind it", got)
		}
		for _, want := range []string{control.LockPath(dir), strconv.Itoa(live), "starting up"} {
			if !strings.Contains(got, want) {
				t.Errorf("heldError = %q, want it to contain %q", got, want)
			}
		}
	})

	t.Run("a bound socket with no recorded holder cannot promise --force", func(t *testing.T) {
		// Serving proves a listener, but with no PID there is nothing to signal
		// and forceTerminate would refuse. The message must not send the user
		// there, and must not invent a PID.
		got := heldError("the default VM", dir, instance.Serving, 0).Error()
		if strings.Contains(got, "br stop --force") {
			t.Errorf("heldError = %q, want no --force promise without a holder PID", got)
		}
		if strings.Contains(got, "pid 0") || strings.Contains(got, "process 0") {
			t.Errorf("heldError = %q, want no invented PID", got)
		}
		if !strings.Contains(got, control.LockPath(dir)) {
			t.Errorf("heldError = %q, want it to name the start lock", got)
		}
	})
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
