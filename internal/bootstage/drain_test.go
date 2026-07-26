package bootstage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestShutdownStageRoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		stage  Stage
		detail string
	}{
		{name: "draining with veto reason", stage: Draining, detail: DetailUnmountRequested},
		{name: "stopping", stage: Stopping},
		{name: "flushing", stage: Flushing},
		{name: "ejecting", stage: Ejecting},
		{name: "clean terminal", stage: Stopped},
		{name: "forced terminal", stage: Forced, detail: DetailForced},
	}
	now := time.Unix(1700000100, 0).UTC()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := WriteDetail(dir, tc.stage, tc.detail, now); err != nil {
				t.Fatalf("WriteDetail: %v", err)
			}
			got, ok := Read(dir)
			if !ok {
				t.Fatal("Read: ok=false after WriteDetail")
			}
			if got.Stage != tc.stage {
				t.Errorf("Stage = %q, want %q", got.Stage, tc.stage)
			}
			if got.Detail != tc.detail {
				t.Errorf("Detail = %q, want %q", got.Detail, tc.detail)
			}
			if !got.UpdatedAt.Equal(now) {
				t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, now)
			}
			if got.Phase != PhaseShutdown {
				t.Errorf("Phase = %q, want %q", got.Phase, PhaseShutdown)
			}
			if got.EffectivePhase() != PhaseShutdown {
				t.Errorf("EffectivePhase = %q, want %q", got.EffectivePhase(), PhaseShutdown)
			}
			wantDescribe := tc.detail
			if wantDescribe == "" {
				wantDescribe = Message(tc.stage)
			}
			if d := Describe(got); d != wantDescribe {
				t.Errorf("Describe = %q, want %q", d, wantDescribe)
			}
		})
	}
}

func TestPhasePredicate(t *testing.T) {
	tests := []struct {
		stage      Stage
		wantPhase  Phase
		isShutdown bool
		isTerminal bool
	}{
		{stage: Boot, wantPhase: PhaseBoot},
		{stage: Setup, wantPhase: PhaseBoot},
		{stage: Connect, wantPhase: PhaseBoot},
		{stage: Incus, wantPhase: PhaseBoot},
		{stage: Ready, wantPhase: PhaseBoot, isTerminal: true},
		{stage: Failed, wantPhase: PhaseBoot, isTerminal: true},
		{stage: Draining, wantPhase: PhaseShutdown, isShutdown: true},
		{stage: Stopping, wantPhase: PhaseShutdown, isShutdown: true},
		{stage: Flushing, wantPhase: PhaseShutdown, isShutdown: true},
		{stage: Ejecting, wantPhase: PhaseShutdown, isShutdown: true},
		{stage: Stopped, wantPhase: PhaseShutdown, isShutdown: true, isTerminal: true},
		{stage: Forced, wantPhase: PhaseShutdown, isShutdown: true, isTerminal: true},
		{stage: "from-the-future", wantPhase: PhaseUnknown},
	}
	for _, tc := range tests {
		t.Run(string(tc.stage), func(t *testing.T) {
			if got := tc.stage.Phase(); got != tc.wantPhase {
				t.Errorf("Phase = %q, want %q", got, tc.wantPhase)
			}
			if got := tc.stage.IsShutdown(); got != tc.isShutdown {
				t.Errorf("IsShutdown = %v, want %v", got, tc.isShutdown)
			}
			if got := tc.stage.IsTerminal(); got != tc.isTerminal {
				t.Errorf("IsTerminal = %v, want %v", got, tc.isTerminal)
			}
		})
	}
}

func TestShutdownRankMonotonic(t *testing.T) {
	for i := 1; i < len(shutdownOrder); i++ {
		if Rank(shutdownOrder[i]) <= Rank(shutdownOrder[i-1]) {
			t.Errorf("Rank(%q)=%d not greater than Rank(%q)=%d",
				shutdownOrder[i], Rank(shutdownOrder[i]), shutdownOrder[i-1], Rank(shutdownOrder[i-1]))
		}
	}
	if Rank(Forced) != -1 {
		t.Errorf("Rank(Forced) = %d, want -1 (unranked terminal)", Rank(Forced))
	}
}

func TestShutdownMessagesNonEmpty(t *testing.T) {
	for _, s := range []Stage{Draining, Stopping, Flushing, Ejecting, Stopped, Forced} {
		if Message(s) == "" {
			t.Errorf("Message(%q) is empty", s)
		}
		if Message(s) == startingMessage {
			t.Errorf("Message(%q) = boot fallback %q", s, startingMessage)
		}
	}
}

// A reader must survive a file written by a newer producer: the stage is
// unknown, but the recorded phase still tells it a spin-down is in progress.
func TestReadUnknownFutureStage(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		wantOK    bool
		wantPhase Phase
		wantDesc  string
	}{
		{
			name:      "unknown shutdown stage with phase",
			raw:       `{"stage":"quiescing","updatedAt":"2023-11-14T22:13:20Z","phase":"shutdown"}`,
			wantOK:    true,
			wantPhase: PhaseShutdown,
			wantDesc:  shuttingDownMessage,
		},
		{
			name:      "unknown stage with unknown phase",
			raw:       `{"stage":"quiescing","updatedAt":"2023-11-14T22:13:20Z","phase":"teleporting"}`,
			wantOK:    true,
			wantPhase: Phase("teleporting"),
			wantDesc:  startingMessage,
		},
		{
			name:      "unknown stage with a detail",
			raw:       `{"stage":"quiescing","updatedAt":"2023-11-14T22:13:20Z","phase":"shutdown","detail":"almost there"}`,
			wantOK:    true,
			wantPhase: PhaseShutdown,
			wantDesc:  "almost there",
		},
		{
			name:      "unrecognized extra fields are ignored",
			raw:       `{"stage":"ejecting","updatedAt":"2023-11-14T22:13:20Z","phase":"shutdown","progress":0.5,"nested":{"a":1}}`,
			wantOK:    true,
			wantPhase: PhaseShutdown,
			wantDesc:  Message(Ejecting),
		},
		{
			name:   "corrupt file reads as no live stage",
			raw:    `{"stage":`,
			wantOK: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeRawState(t, dir, tc.raw)
			got, ok := Read(dir)
			if ok != tc.wantOK {
				t.Fatalf("Read ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if got.EffectivePhase() != tc.wantPhase {
				t.Errorf("EffectivePhase = %q, want %q", got.EffectivePhase(), tc.wantPhase)
			}
			if d := Describe(got); d != tc.wantDesc {
				t.Errorf("Describe = %q, want %q", d, tc.wantDesc)
			}
		})
	}
}

// A file written before the phase/detail fields existed must still read, and
// must still classify as a boot stage.
func TestReadLegacyBootOnlyFile(t *testing.T) {
	for _, stage := range []Stage{Boot, Setup, Connect, Incus, Ready, Failed} {
		t.Run(string(stage), func(t *testing.T) {
			dir := t.TempDir()
			writeRawState(t, dir, `{"stage":"`+string(stage)+`","updatedAt":"2023-11-14T22:13:20Z"}`)
			got, ok := Read(dir)
			if !ok {
				t.Fatal("Read: ok=false for legacy file")
			}
			if got.Stage != stage {
				t.Errorf("Stage = %q, want %q", got.Stage, stage)
			}
			if got.Phase != PhaseUnknown {
				t.Errorf("Phase = %q, want the zero value for a legacy file", got.Phase)
			}
			if got.EffectivePhase() != PhaseBoot {
				t.Errorf("EffectivePhase = %q, want %q", got.EffectivePhase(), PhaseBoot)
			}
			if d := Describe(got); d != Message(stage) {
				t.Errorf("Describe = %q, want %q", d, Message(stage))
			}
		})
	}
}

// Write must keep producing a file an older consumer can parse: the known keys
// keep their names and types, and nothing else is required to be understood.
func TestWriteStaysBackwardCompatible(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1700000000, 0).UTC()
	if err := WriteDetail(dir, Ejecting, "detaching", now); err != nil {
		t.Fatalf("WriteDetail: %v", err)
	}
	b, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// legacyState is the struct shape an older binary compiled against.
	var legacy struct {
		Stage     Stage     `json:"stage"`
		UpdatedAt time.Time `json:"updatedAt"`
	}
	if err := json.Unmarshal(b, &legacy); err != nil {
		t.Fatalf("legacy Unmarshal: %v", err)
	}
	if legacy.Stage != Ejecting {
		t.Errorf("legacy Stage = %q, want %q", legacy.Stage, Ejecting)
	}
	if !legacy.UpdatedAt.Equal(now) {
		t.Errorf("legacy UpdatedAt = %v, want %v", legacy.UpdatedAt, now)
	}
	// A boot-only write must not gain a detail key.
	if err := Write(dir, Boot, now); err != nil {
		t.Fatalf("Write: %v", err)
	}
	b, err = os.ReadFile(Path(dir))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := raw["detail"]; ok {
		t.Errorf("detail key present for a plain Write: %s", b)
	}
	if raw["phase"] != string(PhaseBoot) {
		t.Errorf("phase = %v, want %q", raw["phase"], PhaseBoot)
	}
}

func TestReporterSequence(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1700000200, 0).UTC()
	r := NewReporterWithClock(dir, func() time.Time { return now })

	steps := []struct {
		call      func() error
		wantStage Stage
	}{
		{call: func() error { return r.Draining(DetailUnmountRequested) }, wantStage: Draining},
		{call: func() error { return r.Stopping("") }, wantStage: Stopping},
		{call: func() error { return r.Flushing("") }, wantStage: Flushing},
		{call: func() error { return r.Ejecting("") }, wantStage: Ejecting},
		{call: func() error { return r.Stopped("") }, wantStage: Stopped},
	}
	for _, step := range steps {
		if err := step.call(); err != nil {
			t.Fatalf("report %q: %v", step.wantStage, err)
		}
		got, ok := Read(dir)
		if !ok {
			t.Fatalf("Read: ok=false after %q", step.wantStage)
		}
		if got.Stage != step.wantStage {
			t.Errorf("Stage = %q, want %q", got.Stage, step.wantStage)
		}
		if got.EffectivePhase() != PhaseShutdown {
			t.Errorf("EffectivePhase = %q, want %q", got.EffectivePhase(), PhaseShutdown)
		}
		if r.Stage() != step.wantStage {
			t.Errorf("Reporter.Stage = %q, want %q", r.Stage(), step.wantStage)
		}
		if err := r.Err(); err != nil {
			t.Errorf("Reporter.Err = %v, want nil", err)
		}
	}
	if !r.Stage().IsTerminal() {
		t.Errorf("Stage %q should be terminal", r.Stage())
	}
	r.Clear()
	if _, ok := Read(dir); ok {
		t.Error("Read: ok=true after Reporter.Clear")
	}
	if r.Stage() != "" {
		t.Errorf("Stage = %q after Clear, want empty", r.Stage())
	}
}

func TestReporterMonotonic(t *testing.T) {
	tests := []struct {
		name  string
		calls func(r *Reporter)
		want  Stage
	}{
		{
			name: "a late earlier stage does not move it backwards",
			calls: func(r *Reporter) {
				_ = r.Flushing("")
				_ = r.Draining("")
			},
			want: Flushing,
		},
		{
			name: "the same stage twice stays put",
			calls: func(r *Reporter) {
				_ = r.Stopping("")
				_ = r.Stopping("")
			},
			want: Stopping,
		},
		{
			name: "a power cut is reachable from any stage",
			calls: func(r *Reporter) {
				_ = r.Draining("")
				_ = r.Forced(DetailForced)
			},
			want: Forced,
		},
		{
			name: "nothing follows a power cut",
			calls: func(r *Reporter) {
				_ = r.Forced(DetailForced)
				_ = r.Stopped("")
			},
			want: Forced,
		},
		{
			name: "nothing follows a clean stop",
			calls: func(r *Reporter) {
				_ = r.Stopped("")
				_ = r.Ejecting("")
			},
			want: Stopped,
		},
		{
			name: "a forced finish reports the dirty-filesystem warning",
			calls: func(r *Reporter) {
				_ = r.Draining("")
				_ = r.Finish("forced")
			},
			want: Forced,
		},
		{
			name: "a clean finish reports Stopped",
			calls: func(r *Reporter) {
				_ = r.Draining("")
				_ = r.Finish("clean")
			},
			want: Stopped,
		},
		{
			name:  "an already-stopped guest still finishes cleanly",
			calls: func(r *Reporter) { _ = r.Finish("already-stopped") },
			want:  Stopped,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			r := NewReporter(dir)
			tc.calls(r)
			got, ok := Read(dir)
			if !ok {
				t.Fatal("Read: ok=false")
			}
			if got.Stage != tc.want {
				t.Errorf("Stage = %q, want %q", got.Stage, tc.want)
			}
			if r.Stage() != tc.want {
				t.Errorf("Reporter.Stage = %q, want %q", r.Stage(), tc.want)
			}
			if tc.want == Forced && got.Detail != DetailForced {
				t.Errorf("Detail = %q, want %q", got.Detail, DetailForced)
			}
		})
	}
}

// The DiskArbitration approval callback returns immediately and drains on a
// background goroutine, so reporting races everything else in the holder.
func TestReporterConcurrent(t *testing.T) {
	dir := t.TempDir()
	r := NewReporter(dir)

	const readers = 4
	var wg sync.WaitGroup
	steps := []func() error{
		func() error { return r.Draining(DetailUnmountRequested) },
		func() error { return r.Stopping("") },
		func() error { return r.Flushing("") },
		func() error { return r.Ejecting("") },
		func() error { return r.Stopped("") },
	}
	for _, step := range steps {
		wg.Add(1)
		go func(fn func() error) {
			defer wg.Done()
			if err := fn(); err != nil {
				t.Errorf("report: %v", err)
			}
		}(step)
	}
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				if st, ok := Read(dir); ok {
					if st.EffectivePhase() != PhaseShutdown {
						t.Errorf("EffectivePhase = %q, want %q", st.EffectivePhase(), PhaseShutdown)
					}
					_ = Describe(st)
				}
				_ = r.Stage()
				_ = r.Err()
			}
		}()
	}
	wg.Wait()

	got, ok := Read(dir)
	if !ok {
		t.Fatal("Read: ok=false after concurrent reporting")
	}
	if got.Stage != r.Stage() {
		t.Errorf("file stage %q disagrees with Reporter.Stage %q", got.Stage, r.Stage())
	}
	if Rank(got.Stage) < 0 && !got.Stage.IsTerminal() {
		t.Errorf("Stage = %q, want a known shutdown stage", got.Stage)
	}
}

func TestNilReporterIsUsable(t *testing.T) {
	var r *Reporter
	if err := r.Draining(""); err != nil {
		t.Errorf("Draining on nil Reporter: %v", err)
	}
	if err := r.Finish("forced"); err != nil {
		t.Errorf("Finish on nil Reporter: %v", err)
	}
	if got := r.Stage(); got != "" {
		t.Errorf("Stage = %q, want empty", got)
	}
	if err := r.Err(); err != nil {
		t.Errorf("Err = %v, want nil", err)
	}
	r.Clear()
}

func TestReporterSurfacesWriteError(t *testing.T) {
	r := NewReporter(filepath.Join(t.TempDir(), "does-not-exist"))
	if err := r.Draining(""); err == nil {
		t.Fatal("Draining: want an error writing to a missing directory")
	}
	if r.Err() == nil {
		t.Error("Err = nil, want the last write error")
	}
}

func TestStageForOutcome(t *testing.T) {
	tests := []struct {
		outcome string
		want    Stage
	}{
		{outcome: "clean", want: Stopped},
		{outcome: "already-stopped", want: Stopped},
		{outcome: "not-started", want: Stopped},
		{outcome: "forced", want: Forced},
		{outcome: "", want: Stopped},
	}
	for _, tc := range tests {
		t.Run(tc.outcome, func(t *testing.T) {
			if got := StageForOutcome(tc.outcome); got != tc.want {
				t.Errorf("StageForOutcome(%q) = %q, want %q", tc.outcome, got, tc.want)
			}
		})
	}
}

func writeRawState(t *testing.T, dir, raw string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(raw), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}
