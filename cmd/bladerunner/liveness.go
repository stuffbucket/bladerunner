package main

import (
	"fmt"

	"github.com/stuffbucket/bladerunner/internal/control"
	"github.com/stuffbucket/bladerunner/internal/instance"
)

// This file is the CLI's one answer to "is this instance still up?".
//
// internal/instance owns the question (AGENTS.md section 3) and states it as a
// three-rung ladder: Serving, ProcessOnly, Dead. What the CLI adds is the PID:
// instance.LivenessOf needs one and reads it from a registry Entry, while a
// verb usually holds only a state directory — and the most trustworthy PID for
// a state directory is the one in the start lock beside its control socket.
//
// control.Client.IsRunning is NOT this question. It is a ping round trip, so it
// answers "will this instance reply to me right now", which is a fourth and
// strictly stronger condition than Serving. A holder that is alive but wedged —
// a deadlocked handler, a blocked syscall — keeps its socket bound and keeps
// accepting, it just never replies; the ping times out and IsRunning reports
// false over a VM that still owns its disk, its ports and its cartridge. Use
// IsRunning where a REQUEST is about to be sent and its failure is the answer.
// Use this file where the question is whether anything still holds the
// instance.

// holderPIDAt returns the PID recorded in the start lock of stateDir — the
// process that bound (or is binding) the control socket there — or 0 when there
// is no readable lock.
//
// The lock is the PID source that does not need the control socket to answer:
// it is written before the socket is bound and removed after it is cleaned up,
// so a lock beside a live socket names the process that owns it. See
// control.LockOwnerPID.
func holderPIDAt(stateDir string) int {
	pid, err := control.LockOwnerPID(stateDir)
	if err != nil {
		return 0
	}
	return pid
}

// holderPID returns the PID of the process holding target, taken from a source
// that does not need the control socket to answer.
//
// The start lock comes first because it names the process that owns THIS
// socket. The registry record is the fallback for a holder that could not take
// a lock (see control.LockOwnerPID). Zero means unknown, which forceTerminate
// already refuses to signal.
func holderPID(target resolvedInstance) int {
	if pid := holderPIDAt(target.StateDir); pid > 0 {
		return pid
	}
	return target.PID
}

// livenessOf reports where target sits on instance's liveness ladder, taking
// the PID from the start lock when the record does not carry one.
func livenessOf(target resolvedInstance) instance.Liveness {
	return instance.LivenessOf(instance.Entry{StateDir: target.StateDir, PID: holderPID(target)})
}

// livenessAt reports where the instance rooted at stateDir sits on the ladder,
// for the verbs that hold a directory rather than a resolved instance.
func livenessAt(stateDir string) instance.Liveness {
	return livenessOf(resolvedInstance{StateDir: stateDir})
}

// instanceHeld reports whether anything still holds the instance rooted at
// stateDir: something is serving on its control socket, or a live holder
// process is recorded for it.
//
// This is the rung the "do not start a second one over this" guards need. They
// must be conservative in the direction of refusing, because the alternative is
// two VMM processes on one disk image, and because the false positive they risk
// — a recorded PID that the OS has since recycled onto an unrelated process —
// costs the user a refusal, not their data. Nothing here signals that PID; the
// paths that do (see forceTerminate) demand a successful connect as well.
func instanceHeld(stateDir string) bool {
	return livenessAt(stateDir) != instance.Dead
}

// canForceStop reports whether `br stop --force` has anything it is allowed to
// terminate for an instance on this rung.
//
// This is the ONE definition of that question, because a message that names
// --force and a stop path that refuses to use it are how a user ends up with no
// way out at all. Both halves ask here.
//
// The conjunction is deliberate and neither half is sufficient. Serving proves
// a listener is bound and accepting — a wedged holder accepts from the listen
// backlog even when nothing ever reads — but not which process owns it. A live
// PID proves a process exists, but a holder killed with SIGKILL leaves its lock
// and its registry entry behind, so once the OS reuses that PID it names an
// innocent process. Only together do they say "this PID is behind that
// listener".
func canForceStop(rung instance.Liveness, pid int) bool {
	return rung == instance.Serving && instance.ProcessAlive(pid)
}

// heldError explains an instance that something still holds but that will not
// answer, with the remedy that applies to the rung it is actually on.
//
// The two live rungs need OPPOSITE advice, which is why this branches rather
// than printing one message. On Serving there is a listener and a live PID
// behind it, so --force can prove the holder is there and terminate it. On
// ProcessOnly there is no listener at all — only a PID in the start lock — and
// --force refuses to signal that, correctly. Naming --force there sent the user
// to a command that answers "VM is not running" and does nothing, while every
// guard in the CLI went on refusing: a closed loop with no exit.
func heldError(label, stateDir string, rung instance.Liveness, pid int) error {
	if canForceStop(rung, pid) {
		return fmt.Errorf("%s is unresponsive: holder process %d is alive but its control socket %s is not answering\n"+
			"  terminate it with 'br stop --force'", label, pid, control.SocketPath(stateDir))
	}
	return fmt.Errorf("%s is held but not serving: %s", label, heldWithoutListenerNote(stateDir, pid))
}

// heldWithoutListenerNote explains the ProcessOnly rung and names the only
// thing that clears it.
//
// Removing the start lock is the escape, and it is enough on its own: the
// leftover socket file beside it does not have to be removed by hand, because
// control's bindListener unlinks a stale socket itself — but only after it wins
// the lock. TestRemovingTheStartLockClearsAHeldInstance holds that claim.
//
// The advice stops short of removing the lock automatically. A live recorded
// PID has two meanings that nothing on this host can tell apart: an instance
// still starting up, whose lock is the only thing keeping a second holder off
// the disk image it is about to open, or a dead holder's litter with a recycled
// PID. The lock is an O_CREAT|O_EXCL file holding a number, not a kernel-held
// flock, so it carries no evidence of which. Deleting it on a guess is an
// AGENTS.md section 8 operation; naming it costs the user one command and
// cannot corrupt anything.
func heldWithoutListenerNote(stateDir string, pid int) string {
	socket := control.SocketPath(stateDir)
	lock := control.LockPath(stateDir)
	if pid <= 0 {
		return fmt.Sprintf("nothing is listening on %s and %s records no usable holder\n"+
			"  remove %s and try again", socket, lock, lock)
	}
	return fmt.Sprintf("nothing is listening on %s, and %s records pid %d as its holder\n"+
		"  if pid %d is this instance starting up, wait for it and try again\n"+
		"  if it is not bladerunner — a crashed holder's pid gets reused — remove %s and try again",
		socket, lock, pid, pid, lock)
}
