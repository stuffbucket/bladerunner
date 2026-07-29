package vmhost

import (
	"context"
	"testing"
	"time"
)

// TestInfoIsRaceFreeAgainstRunFromAPreExistingGoroutine pins the fix for a race
// that production ordering happened to hide.
//
// Info is documented as safe to call from any goroutine, and Run writes
// h.startedAt. Every goroutine that calls Info in production today is created
// BY A STEP — that is, after the write — so a happens-before edge exists by
// accident of ordering, and the race detector never sees it. A caller that
// holds a *Host and polls Info() from a goroutine started BEFORE Run has no
// such edge.
//
// This test creates exactly that shape: a reader goroutine is running before
// Run is entered. Against an unsynchronized startedAt it fails under -race with
// a WARNING: DATA RACE naming the write in Run and the read in Info.
func TestInfoIsRaceFreeAgainstRunFromAPreExistingGoroutine(t *testing.T) {
	hold := step{
		name:  "hold",
		start: func(context.Context) error { return nil },
		stop:  func() error { return nil },
	}
	h, _ := newFakeHost(t, &recorder{}, hold)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The reader starts BEFORE Run. This is the ordering production does not
	// currently produce and therefore never trips over.
	readerDone := make(chan struct{})
	stopReading := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-stopReading:
				return
			default:
				_ = h.Info().StartedAt
			}
		}
	}()

	// Give the reader a moment to be genuinely in flight before Run writes.
	time.Sleep(10 * time.Millisecond)

	runDone := make(chan error, 1)
	go func() { runDone <- h.Run(ctx) }()

	time.Sleep(50 * time.Millisecond)
	cancel()
	<-runDone
	close(stopReading)
	<-readerDone
}

// TestRunStampsStartedAtVisibly checks the accessor actually reports the value
// Run recorded, so guarding the field did not turn Info's StartedAt into a
// permanent zero.
func TestRunStampsStartedAtVisibly(t *testing.T) {
	before := time.Now()
	seen := make(chan time.Time, 1)
	var h *Host
	observe := step{
		name: "observe",
		start: func(context.Context) error {
			seen <- h.Info().StartedAt
			return nil
		},
		stop: func() error { return nil },
	}
	h, _ = newFakeHost(t, &recorder{}, observe)

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- h.Run(ctx) }()

	var got time.Time
	select {
	case got = <-seen:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("the step never ran")
	}
	cancel()
	<-runDone

	if got.IsZero() {
		t.Fatal("Info().StartedAt is zero inside a step; Run must stamp it first")
	}
	if got.Before(before) {
		t.Errorf("Info().StartedAt = %v, which predates the run (%v)", got, before)
	}
}
