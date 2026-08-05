package bootstage

import (
	"sync"
	"time"
)

// Shutdown-phase stages, ordered earliest-to-latest. A spin-down can
// legitimately take the whole drain budget (internal/vm.DefaultDrainTimeout is
// 60s), so the user needs to see which step it is on rather than a hang — in
// particular when the gesture that started it was hitting eject in Finder and
// the unmount was vetoed with "busy".
const (
	Draining Stage = "draining" // guest asked to power off (ACPI)
	Stopping Stage = "stopping" // waiting for the stopped transition
	Flushing Stage = "flushing" // fsync/detach of the backing disks
	Ejecting Stage = "ejecting" // unmounting the cartridge volume
	Stopped  Stage = "stopped"  // guest powered itself off cleanly
	Forced   Stage = "forced"   // the VMM was force-stopped (a power cut)
	// Stuck is the terminal for a spin-down that could not release the volume:
	// the guest is down but the cartridge image is STILL ATTACHED because the
	// detach failed. It is the highest-consequence state in this vocabulary —
	// the user is standing over a physical object they are about to pull out —
	// so it is published rather than left to silence. An eject that goes quiet
	// reads as finished, which invites exactly the pull that must not happen.
	Stuck Stage = "stuck"
)

// shuttingDownMessage is the fallback line for a shutdown-phase stage this
// binary does not know.
const shuttingDownMessage = "Shutting down…"

// Details a producer can hand to a Reporter. They are plain sentences because
// they are shown verbatim to the user.
const (
	// DetailUnmountRequested explains a vetoed eject. It matches the reason
	// string the DiskArbitration dissenter hands to Finder, so the notice in
	// the menubar reads the same as the one in the eject dialog.
	DetailUnmountRequested = "bladerunner is shutting down the VM on this cartridge"
	// DetailForced warns that a power cut may have left the guest filesystem
	// dirty. Surfaced on the Forced stage.
	DetailForced = "the VM did not stop in time and was powered off — the guest filesystem may need a check"
)

// shutdownOrder lists the rankable shutdown stages earliest-to-latest; rank is
// derived from the index so the ordering lives in one place (Forced is a
// terminal that can be reached from anywhere, so it is unranked).
//
// Stuck sits LAST, above Stopped, although the two are alternatives rather than
// a sequence — a detach either releases the volume or it does not. Both are
// terminal, so advancesShutdown already refuses to move between them and the
// relative rank should never be consulted. It is ordered this way so that if
// that guard is ever weakened, the move that stays possible is the one towards
// the safety warning, not away from it.
var shutdownOrder = []Stage{Draining, Stopping, Flushing, Ejecting, Stopped, Stuck}

// shutdownMessage maps the shutdown stages to their friendly line, reporting
// ok=false for anything outside this phase so Message keeps its own fallbacks.
func shutdownMessage(s Stage) (string, bool) {
	switch s {
	case Draining:
		return "Shutting down the VM…", true
	case Stopping:
		return "Waiting for the VM to stop…", true
	case Flushing:
		return "Flushing disks…", true
	case Ejecting:
		return "Ejecting cartridge…", true
	case Stopped:
		return "Stopped", true
	case Forced:
		return "Force-stopped — the disk may need a check", true
	case Stuck:
		// Message is the ENTIRE user-visible text: the menubar and the splash
		// both render Message(Stage) and neither renders Detail, so the
		// instruction has to be here. It says what to DO — do not pull it, and
		// what will release it — rather than reporting that a detach failed,
		// which tells the user nothing they can act on.
		return "Do not remove the cartridge — try ejecting it again", true
	default:
		return "", false
	}
}

// StageForOutcome maps a drain outcome to its terminal stage. The argument is
// the string form of an internal/vm.StopOutcome; it is taken as a string so
// this package stays dependency-free and buildable off darwin. Anything other
// than a forced power cut reports a clean Stopped.
func StageForOutcome(outcome string) Stage {
	if outcome == string(Forced) {
		return Forced
	}
	return Stopped
}

// Reporter publishes shutdown-phase stages for one instance's state directory.
//
// It is safe for concurrent use from any goroutine: the DiskArbitration
// unmount-approval callback must return immediately, so it dissents and then
// runs the drain on a background goroutine while the control plane, the menubar
// bridge and the signal handler are all still live.
//
// Transitions are monotonic within the shutdown phase — a late report cannot
// move the displayed stage backwards — except for Forced, which is reachable
// from anywhere because a power cut can interrupt any step. Writes are
// best-effort: each method returns the write error for logging, and the last
// error is also available from Err.
//
// A nil *Reporter is usable and does nothing, so a caller with no state
// directory (tests, one-shot tooling) need not branch.
type Reporter struct {
	stateDir string
	now      func() time.Time

	mu   sync.Mutex
	cur  Stage
	last error
}

// NewReporter returns a Reporter writing the stage file under stateDir.
func NewReporter(stateDir string) *Reporter {
	return NewReporterWithClock(stateDir, time.Now)
}

// NewReporterWithClock is NewReporter with an injected clock, matching Write's
// injected now so timestamps stay testable.
func NewReporterWithClock(stateDir string, now func() time.Time) *Reporter {
	if now == nil {
		now = time.Now
	}
	return &Reporter{stateDir: stateDir, now: now}
}

// Draining records that the guest has been asked to power itself off. detail
// may be empty to use the canned message; the unmount-veto path passes
// DetailUnmountRequested.
func (r *Reporter) Draining(detail string) error { return r.report(Draining, detail) }

// Stopping records that the drain is waiting for the VM to reach the stopped
// state.
func (r *Reporter) Stopping(detail string) error { return r.report(Stopping, detail) }

// Flushing records that the disks are being synced and released.
func (r *Reporter) Flushing(detail string) error { return r.report(Flushing, detail) }

// Ejecting records that the cartridge volume is being unmounted and detached.
func (r *Reporter) Ejecting(detail string) error { return r.report(Ejecting, detail) }

// Stopped records the clean terminal: the guest powered itself off.
func (r *Reporter) Stopped(detail string) error { return r.report(Stopped, detail) }

// Forced records the unclean terminal: the VMM was power-cut. detail may be
// empty to use the canned message; callers that want to warn about a dirty
// guest filesystem pass DetailForced.
func (r *Reporter) Forced(detail string) error { return r.report(Forced, detail) }

// Stuck records that the guest is down but the cartridge could NOT be released:
// the volume is still attached and must not be pulled. detail may be empty to
// use the canned message, which is what both consumers render.
func (r *Reporter) Stuck(detail string) error { return r.report(Stuck, detail) }

// Finish records the terminal stage matching a drain outcome (the string form
// of an internal/vm.StopOutcome), attaching DetailForced to a power cut.
func (r *Reporter) Finish(outcome string) error {
	stage := StageForOutcome(outcome)
	if stage == Forced {
		return r.Forced(DetailForced)
	}
	return r.Stopped("")
}

// Stage reports the last stage this Reporter published, or "" if none.
func (r *Reporter) Stage() Stage {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cur
}

// Err reports the most recent write error, or nil if the last write succeeded.
func (r *Reporter) Err() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// Clear removes the stage file and forgets the published stage, so the next
// lifecycle starts from nothing.
func (r *Reporter) Clear() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	Clear(r.stateDir)
	r.cur = ""
	r.last = nil
}

// report writes stage unless it would move the published stage backwards. The
// lock is held across the write so concurrent reporters cannot interleave and
// leave an earlier stage as the last writer.
func (r *Reporter) report(stage Stage, detail string) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !advancesShutdown(r.cur, stage) {
		return nil
	}
	err := WriteState(r.stateDir, State{Stage: stage, UpdatedAt: r.now(), Detail: detail})
	r.cur, r.last = stage, err
	return err
}

// advancesShutdown reports whether moving from cur to next is a forward move.
// Forced is always allowed (a power cut can interrupt any step) and is final;
// otherwise the per-phase rank must strictly increase.
func advancesShutdown(cur, next Stage) bool {
	switch {
	case next == Forced:
		return cur != Forced
	case cur == "":
		return true
	case cur.IsTerminal():
		return false
	default:
		return Rank(next) > Rank(cur)
	}
}
