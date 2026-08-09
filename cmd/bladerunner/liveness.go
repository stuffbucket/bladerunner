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

// unresponsiveError reports an instance that is held but not answering: the
// state a ping cannot tell apart from "not running", and the one `br stop
// --force` exists for. It names the holder so the user can confirm what they
// are about to terminate.
func unresponsiveError(label, stateDir string) error {
	pid := holderPIDAt(stateDir)
	if pid <= 0 {
		return fmt.Errorf("%s is unresponsive: its control socket %s is not answering\n"+
			"  terminate it with 'br stop --force'", label, control.SocketPath(stateDir))
	}
	return fmt.Errorf("%s is unresponsive: holder process %d is alive but its control socket %s is not answering\n"+
		"  terminate it with 'br stop --force'", label, pid, control.SocketPath(stateDir))
}
