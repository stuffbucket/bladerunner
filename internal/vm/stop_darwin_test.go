//go:build darwin

package vm

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Code-Hex/vz/v3"
)

// fakeVM implements drainTarget so the drain state machine can be exercised
// without a real VZ virtual machine. Transitions are driven by the test through
// onRequestStop / onForceStop, which stand in for the guest reacting (or not)
// to the ACPI power button.
type fakeVM struct {
	mu    sync.Mutex
	state vz.VirtualMachineState
	ch    chan vz.VirtualMachineState

	requestStops int
	forceStops   int
	requestErr   error

	canRequestStop bool
	canStop        bool

	onRequestStop func(f *fakeVM)
	onForceStop   func(f *fakeVM)
}

func newFakeVM(state vz.VirtualMachineState) *fakeVM {
	return &fakeVM{
		state:          state,
		ch:             make(chan vz.VirtualMachineState, 4),
		canRequestStop: true,
		canStop:        true,
	}
}

func (f *fakeVM) State() vz.VirtualMachineState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *fakeVM) StateChangedNotify() <-chan vz.VirtualMachineState { return f.ch }

func (f *fakeVM) CanRequestStop() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.canRequestStop
}

func (f *fakeVM) CanStop() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.canStop
}

func (f *fakeVM) RequestStop() (bool, error) {
	f.mu.Lock()
	f.requestStops++
	err := f.requestErr
	f.mu.Unlock()
	if f.onRequestStop != nil {
		f.onRequestStop(f)
	}
	return err == nil, err
}

func (f *fakeVM) Stop() error {
	f.mu.Lock()
	f.forceStops++
	f.mu.Unlock()
	if f.onForceStop != nil {
		f.onForceStop(f)
	}
	return nil
}

// notify publishes a state change the way VZ does, without changing State() —
// the drain must react to the notification, not only to polling.
func (f *fakeVM) notify(state vz.VirtualMachineState) { f.ch <- state }

// settle marks the machine stopped and publishes the transition.
func (f *fakeVM) settle() {
	f.mu.Lock()
	f.state = vz.VirtualMachineStateStopped
	f.canRequestStop = false
	f.mu.Unlock()
	f.notify(vz.VirtualMachineStateStopped)
}

func (f *fakeVM) counts() (requests, forces int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requestStops, f.forceStops
}

// quietLogger swallows drain logging during tests.
type quietLogger struct{}

func (quietLogger) Info(any, ...any) {}
func (quietLogger) Warn(any, ...any) {}

func TestDrainGuest(t *testing.T) {
	const shortBudget = 50 * time.Millisecond

	tests := []struct {
		name         string
		setup        func() *fakeVM
		budget       time.Duration
		force        bool
		wantOutcome  StopOutcome
		wantRequests int
		wantForces   int
		wantErr      bool
	}{
		{
			name: "stops cleanly when the guest powers off within budget",
			setup: func() *fakeVM {
				f := newFakeVM(vz.VirtualMachineStateRunning)
				f.onRequestStop = func(f *fakeVM) { f.settle() }
				return f
			},
			budget:       time.Second,
			wantOutcome:  StopOutcomeClean,
			wantRequests: 1,
			wantForces:   0,
		},
		{
			name: "stops cleanly on a stopped notification alone",
			setup: func() *fakeVM {
				f := newFakeVM(vz.VirtualMachineStateRunning)
				// State() keeps reporting running; only the notification arrives.
				f.onRequestStop = func(f *fakeVM) { f.notify(vz.VirtualMachineStateStopped) }
				return f
			},
			budget:       time.Second,
			wantOutcome:  StopOutcomeClean,
			wantRequests: drainRequestStopAttempts,
			wantForces:   0,
		},
		{
			name: "force-escalates exactly once when the guest ignores ACPI",
			setup: func() *fakeVM {
				f := newFakeVM(vz.VirtualMachineStateRunning)
				f.onForceStop = func(f *fakeVM) { f.settle() }
				return f
			},
			budget:       shortBudget,
			wantOutcome:  StopOutcomeForced,
			wantRequests: drainRequestStopAttempts,
			wantForces:   1,
		},
		{
			name: "never forces a VM that is already stopped",
			setup: func() *fakeVM {
				f := newFakeVM(vz.VirtualMachineStateStopped)
				return f
			},
			budget:       shortBudget,
			wantOutcome:  StopOutcomeAlreadyStopped,
			wantRequests: 0,
			wantForces:   0,
		},
		{
			name: "never asks a stopped VM to power off even when force is requested",
			setup: func() *fakeVM {
				return newFakeVM(vz.VirtualMachineStateStopped)
			},
			budget:      shortBudget,
			force:       true,
			wantOutcome: StopOutcomeAlreadyStopped,
		},
		{
			name: "force skips the ACPI request entirely",
			setup: func() *fakeVM {
				f := newFakeVM(vz.VirtualMachineStateRunning)
				f.onForceStop = func(f *fakeVM) { f.settle() }
				return f
			},
			budget:       time.Second,
			force:        true,
			wantOutcome:  StopOutcomeForced,
			wantRequests: 0,
			wantForces:   1,
		},
		{
			name: "reports an error when even the forced stop does not settle",
			setup: func() *fakeVM {
				return newFakeVM(vz.VirtualMachineStateRunning)
			},
			budget:       shortBudget,
			force:        true,
			wantOutcome:  StopOutcomeForced,
			wantRequests: 0,
			wantForces:   1,
			wantErr:      true,
		},
		{
			name: "surfaces the error state instead of waiting out the budget",
			setup: func() *fakeVM {
				f := newFakeVM(vz.VirtualMachineStateError)
				f.canRequestStop = false
				return f
			},
			budget:      time.Second,
			wantOutcome: StopOutcomeForced,
			wantForces:  1,
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := tt.setup()
			// Bound the post-escalation grace (a fixed 10s in production) so a
			// case that never settles still finishes quickly.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			outcome, err := drainGuest(ctx, f, tt.budget, tt.force, quietLogger{})

			if outcome != tt.wantOutcome {
				t.Errorf("outcome = %q, want %q", outcome, tt.wantOutcome)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			requests, forces := f.counts()
			if requests != tt.wantRequests {
				t.Errorf("RequestStop calls = %d, want %d", requests, tt.wantRequests)
			}
			if forces != tt.wantForces {
				t.Errorf("Stop (force) calls = %d, want %d", forces, tt.wantForces)
			}
		})
	}
}

// TestDrainGuestStopsRequestingWhenRefused verifies the ACPI loop gives up as
// soon as the VM stops accepting requests, rather than spinning the full three
// attempts.
func TestDrainGuestStopsRequestingWhenRefused(t *testing.T) {
	f := newFakeVM(vz.VirtualMachineStateRunning)
	f.canRequestStop = false
	f.onForceStop = func(f *fakeVM) { f.settle() }

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	outcome, err := drainGuest(ctx, f, 50*time.Millisecond, false, quietLogger{})
	if err != nil {
		t.Fatalf("drainGuest: %v", err)
	}
	if outcome != StopOutcomeForced {
		t.Fatalf("outcome = %q, want %q", outcome, StopOutcomeForced)
	}
	if requests, _ := f.counts(); requests != 0 {
		t.Errorf("RequestStop calls = %d, want 0", requests)
	}
}

// TestDrainNoticesStopWithoutNotification covers the case where another
// consumer of VZ's shared state-change channel (Runner.Wait) takes the stopped
// event: the drain must still see the machine stop by polling, and must not
// force-stop an already-stopped VM.
func TestDrainNoticesStopWithoutNotification(t *testing.T) {
	f := newFakeVM(vz.VirtualMachineStateRunning)
	f.onRequestStop = func(f *fakeVM) {
		f.mu.Lock()
		f.canRequestStop = false
		f.mu.Unlock()
		// Settle silently: the state changes, but the notification never arrives.
		go func() {
			time.Sleep(2 * drainStatePollInterval)
			f.mu.Lock()
			f.state = vz.VirtualMachineStateStopped
			f.mu.Unlock()
		}()
	}

	outcome, err := drainGuest(context.Background(), f, 5*time.Second, false, quietLogger{})
	if err != nil {
		t.Fatalf("drainGuest: %v", err)
	}
	if outcome != StopOutcomeClean {
		t.Fatalf("outcome = %q, want %q", outcome, StopOutcomeClean)
	}
	if _, forces := f.counts(); forces != 0 {
		t.Errorf("Stop (force) calls = %d, want 0", forces)
	}
}

func TestStopOnUnstartedRunnerIsCleanAndIdempotent(t *testing.T) {
	r := &Runner{}
	if err := r.Stop(); err != nil {
		t.Fatalf("Stop on an unstarted runner: %v", err)
	}
	if got := r.LastStopOutcome(); got != StopOutcomeNotStarted {
		t.Errorf("LastStopOutcome = %q, want %q", got, StopOutcomeNotStarted)
	}
	if err := r.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestSyncDiskImage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "root.img")
	if err := os.WriteFile(path, []byte("disk"), 0o600); err != nil {
		t.Fatalf("write image: %v", err)
	}

	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{name: "existing image", path: path},
		{name: "empty path is a no-op"},
		{name: "missing image is a no-op", path: filepath.Join(dir, "absent.img")},
		{name: "unreadable path", path: dir, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := SyncDiskImage(tt.path)
			if (err != nil) != tt.wantErr {
				t.Fatalf("SyncDiskImage(%q) = %v, wantErr = %v", tt.path, err, tt.wantErr)
			}
		})
	}
}
