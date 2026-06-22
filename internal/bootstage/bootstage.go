// Package bootstage publishes a coarse, human-friendly boot phase to a small
// JSON file so a separate process (the menubar) can show live "what is the VM
// doing" status on the splash without the rich in-process progress board.
//
// The producer is `br start` (which has the console log + runner events); the
// consumer is the menubar, which polls Read while the starting splash is up.
package bootstage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Stage is a coarse boot phase, ordered from earliest to latest. Producers
// advance monotonically; the numeric Rank guards against regressions when two
// signals (console parse vs runner event) arrive out of order.
type Stage string

const (
	Boot    Stage = "boot"    // VM/kernel coming up
	Setup   Stage = "setup"   // cloud-init configuring the guest
	Connect Stage = "connect" // cloud-init done, bringing up SSH
	Incus   Stage = "incus"   // guest reachable, waiting on the Incus API
	Ready   Stage = "ready"   // fully up
	Failed  Stage = "failed"  // boot failed (e.g. cloud-init error)
)

const fileName = "boot-stage.json"

// stageOrder lists the rankable stages earliest-to-latest; rank is derived from
// the index so the ordering lives in one place (Failed is terminal, unranked).
var stageOrder = []Stage{Boot, Setup, Connect, Incus, Ready}

// rank orders the non-terminal stages so a producer can refuse to move
// backwards. Failed/Ready are terminal and bypass the rank check.
var rank = func() map[Stage]int {
	m := make(map[Stage]int, len(stageOrder))
	for i, s := range stageOrder {
		m[s] = i
	}
	return m
}()

// Rank returns the monotonic ordering of s (terminal Ready highest). Unknown
// stages rank -1.
func Rank(s Stage) int {
	if r, ok := rank[s]; ok {
		return r
	}
	return -1
}

// Message maps a stage to the friendly, non-technical line shown on the splash
// and in the menu. Unknown stages fall back to a generic "Starting…".
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
		return "Starting…"
	}
}

// State is the persisted boot phase plus when it was written, so a stale file
// from a previous run can be ignored by age.
type State struct {
	Stage     Stage     `json:"stage"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Path returns the boot-stage file location under stateDir.
func Path(stateDir string) string {
	return filepath.Join(stateDir, fileName)
}

// Write atomically records stage at the current time (now is injected so the
// menubar's stale-age check stays testable). Temp-file + rename so a reader
// never sees a half-written file.
func Write(stateDir string, stage Stage, now time.Time) error {
	b, err := json.Marshal(State{Stage: stage, UpdatedAt: now})
	if err != nil {
		return err
	}
	path := Path(stateDir)
	tmp, err := os.CreateTemp(stateDir, fileName+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// Read returns the recorded state, or ok=false when the file is absent or
// unreadable/corrupt (treated the same — there is simply no live stage).
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
