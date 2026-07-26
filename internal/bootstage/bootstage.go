// Package bootstage publishes a coarse, human-friendly lifecycle phase to a
// small JSON file so a separate process (the menubar) can show live "what is
// the VM doing" status on the splash without the rich in-process progress
// board.
//
// The file covers both halves of the lifecycle: the boot phase (produced by
// `br start`, which has the console log + runner events) and the shutdown phase
// (produced by the drain path in the holder — see drain.go, whose Reporter is
// safe to call from the background goroutine a DiskArbitration unmount-approval
// callback kicks off). The consumer is the menubar, which polls Read while the
// splash or the spin-down notice is up.
package bootstage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/stuffbucket/bladerunner/internal/util"
)

// Stage is a coarse lifecycle phase. Within a phase the stages are ordered
// earliest to latest; producers advance monotonically, and the numeric Rank
// guards against regressions when two signals (console parse vs runner event)
// arrive out of order. Use Stage.Phase to tell a boot stage from a shutdown
// stage — never string matching.
type Stage string

const (
	Boot    Stage = "boot"    // VM/kernel coming up
	Setup   Stage = "setup"   // cloud-init configuring the guest
	Connect Stage = "connect" // cloud-init done, bringing up SSH
	Incus   Stage = "incus"   // guest reachable, waiting on the Incus API
	Ready   Stage = "ready"   // fully up
	Failed  Stage = "failed"  // boot failed (e.g. cloud-init error)
)

// Phase names the half of the lifecycle a stage belongs to, so a reader can
// distinguish "coming up" from "going down" without knowing every stage name —
// including stages added by a newer producer than the reader.
type Phase string

const (
	// PhaseUnknown is the zero value: a stage this binary does not know, or a
	// state file written before the phase field existed.
	PhaseUnknown Phase = ""
	// PhaseBoot covers the start-up half of the lifecycle.
	PhaseBoot Phase = "boot"
	// PhaseShutdown covers the drain/eject half of the lifecycle.
	PhaseShutdown Phase = "shutdown"
)

const fileName = "boot-stage.json"

// statePerm is the mode of the boot-stage file: readable by anything that can
// read the state dir (the menubar polls it), writable only by its owner.
const statePerm = 0o600

// startingMessage is the fallback line for a stage this binary does not know
// that is (or is assumed to be) part of the boot phase.
const startingMessage = "Starting…"

// stageOrder lists the rankable boot stages earliest-to-latest; rank is derived
// from the index so the ordering lives in one place (Failed is terminal,
// unranked). The shutdown counterpart is shutdownOrder in drain.go.
var stageOrder = []Stage{Boot, Setup, Connect, Incus, Ready}

// terminalStages are the stages no further transition follows, per phase.
var terminalStages = map[Stage]bool{Ready: true, Failed: true, Stopped: true, Forced: true}

// rank orders the non-terminal stages of each phase so a producer can refuse to
// move backwards. Ranks are per-phase: comparing a boot stage's rank with a
// shutdown stage's rank is meaningless, so compare Phase first.
var rank = func() map[Stage]int {
	m := make(map[Stage]int, len(stageOrder)+len(shutdownOrder))
	for i, s := range stageOrder {
		m[s] = i
	}
	for i, s := range shutdownOrder {
		m[s] = i
	}
	return m
}()

// phaseOf classifies every stage this binary knows about.
var phaseOf = func() map[Stage]Phase {
	m := make(map[Stage]Phase, len(stageOrder)+len(shutdownOrder)+1)
	for _, s := range stageOrder {
		m[s] = PhaseBoot
	}
	m[Failed] = PhaseBoot
	for _, s := range shutdownOrder {
		m[s] = PhaseShutdown
	}
	m[Forced] = PhaseShutdown
	return m
}()

// Rank returns the monotonic ordering of s within its own phase (the ranked
// terminal of each phase — Ready, Stopped — is highest). Unranked terminals
// (Failed, Forced) and unknown stages rank -1. Ranks are only comparable
// between stages of the same Phase.
func Rank(s Stage) int {
	if r, ok := rank[s]; ok {
		return r
	}
	return -1
}

// Phase reports which half of the lifecycle s belongs to, or PhaseUnknown for a
// stage this binary does not know.
func (s Stage) Phase() Phase { return phaseOf[s] }

// IsShutdown reports whether s belongs to the drain/eject half of the
// lifecycle. This is the predicate a consumer should branch on rather than
// matching stage names.
func (s Stage) IsShutdown() bool { return s.Phase() == PhaseShutdown }

// IsTerminal reports whether s is an end state, so a poller knows to stop
// waiting for further transitions.
func (s Stage) IsTerminal() bool { return terminalStages[s] }

// Message maps a stage to the friendly, non-technical line shown on the splash
// and in the menu. Unknown stages fall back to a generic phase-appropriate
// line.
func Message(s Stage) string {
	switch s {
	case Boot:
		return "Booting Linux…"
	case Setup:
		return "Setting up…"
	case Connect:
		return "Connecting…"
	case Incus:
		return "Starting Incus…"
	case Ready:
		return "Ready"
	case Failed:
		return "Boot failed — check logs"
	default:
		if m, ok := shutdownMessage(s); ok {
			return m
		}
		if s.IsShutdown() {
			return shuttingDownMessage
		}
		return startingMessage
	}
}

// State is the persisted lifecycle phase plus when it was written, so a stale
// file from a previous run can be ignored by age.
//
// Phase and Detail were added with the shutdown stages and are omitted when
// empty: a file written by an older producer simply lacks them, and an older
// consumer ignores them. Never remove or reorder the existing fields.
type State struct {
	Stage     Stage     `json:"stage"`
	UpdatedAt time.Time `json:"updatedAt"`
	// Phase records Stage's half of the lifecycle at write time so a consumer
	// that does not know a newer stage can still tell boot from shutdown.
	Phase Phase `json:"phase,omitempty"`
	// Detail is an optional human-readable reason shown instead of the canned
	// message — e.g. why an eject was vetoed, or that a forced stop may have
	// left the guest filesystem needing a check.
	Detail string `json:"detail,omitempty"`
}

// EffectivePhase returns the recorded phase, falling back to the phase derived
// from Stage when the file predates the field. A file naming a stage this
// binary does not know still reports the right phase from the recorded value.
func (s State) EffectivePhase() Phase {
	if s.Phase != PhaseUnknown {
		return s.Phase
	}
	return s.Stage.Phase()
}

// Describe returns the line to show for s: the recorded detail when present,
// otherwise the stage's canned message. Unlike Message it can fall back on the
// recorded phase, so an unknown shutdown stage does not read as "Starting…".
func Describe(s State) string {
	if s.Detail != "" {
		return s.Detail
	}
	if s.Stage.Phase() == PhaseUnknown && s.EffectivePhase() == PhaseShutdown {
		return shuttingDownMessage
	}
	return Message(s.Stage)
}

// Path returns the boot-stage file location under stateDir.
func Path(stateDir string) string {
	return filepath.Join(stateDir, fileName)
}

// Write atomically records stage at the current time (now is injected so the
// menubar's stale-age check stays testable). Temp-file + rename so a reader
// never sees a half-written file.
func Write(stateDir string, stage Stage, now time.Time) error {
	return WriteState(stateDir, State{Stage: stage, UpdatedAt: now})
}

// WriteState atomically records s, filling in Phase from Stage when unset.
// The write is temp-file + fsync + rename + directory fsync (see
// util.WriteFileAtomic), so a reader never sees a half-written file and the
// rename survives a crash.
func WriteState(stateDir string, s State) error {
	if s.Phase == PhaseUnknown {
		s.Phase = s.Stage.Phase()
	}
	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("encode boot stage %q: %w", s.Stage, err)
	}
	return util.WriteFileAtomic(Path(stateDir), b, statePerm)
}

// Read returns the recorded state, or ok=false when the file is absent or
// unreadable/corrupt (treated the same — there is simply no live stage). A
// stage this binary does not know is returned as-is: consumers should branch on
// EffectivePhase and Describe rather than on the stage name.
func Read(stateDir string) (State, bool) {
	b, err := os.ReadFile(Path(stateDir))
	if err != nil {
		return State{}, false
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil || s.Stage == "" {
		return State{}, false
	}
	return s, true
}

// Clear removes the boot-stage file (best effort). Called when the VM stops so
// a stale phase never lingers.
func Clear(stateDir string) {
	_ = os.Remove(Path(stateDir))
}
