package vmhost

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stuffbucket/bladerunner/internal/config"
)

// blockBudget bounds each case so a supervisor that never fires fails the test
// instead of hanging the suite.
const blockBudget = 5 * time.Second

// runBlock drives the headless branch of block with the guest supervisor
// substituted, and reports what block returned.
func runBlock(t *testing.T, wait func(context.Context) error, cancelAfter time.Duration) error {
	t.Helper()
	h := &Host{
		cfg:       &config.Config{VMDir: t.TempDir()},
		obs:       NopObserver{},
		waitReady: func(context.Context) error { return nil },
		waitVM:    wait,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if cancelAfter > 0 {
		go func() { time.Sleep(cancelAfter); cancel() }()
	}

	done := make(chan error, 1)
	go func() { done <- h.block(ctx) }()
	select {
	case err := <-done:
		return err
	case <-time.After(blockBudget):
		t.Fatalf("block did not return within %v", blockBudget)
		return nil
	}
}

// The triple. A guest that stops cleanly and a guest that dies must BOTH end
// the block — a holder that outlives its guest reports the instance as running
// through a control socket the dead guest can no longer back. The two are
// distinguished by block's error. The canceled case is the regression guard:
// the ordinary stop path must behave exactly as it did before.
func TestBlockReleasesTheHolderWhenTheGuestGoes(t *testing.T) {
	t.Run("a clean power-off ends the block without an error", func(t *testing.T) {
		if err := runBlock(t, func(context.Context) error { return nil }, 0); err != nil {
			t.Fatalf("block = %v, want nil for a guest that powered itself off", err)
		}
	})

	t.Run("a dead guest ends the block and is distinguishable", func(t *testing.T) {
		err := runBlock(t, func(context.Context) error { return errors.New("vm entered error state") }, 0)
		if err == nil {
			t.Fatal("block returned nil for a guest that entered the error state; the holder would keep reporting it as running")
		}
		if !strings.Contains(err.Error(), "vm entered error state") {
			t.Errorf("block = %v, want it to name why the guest stopped", err)
		}
	})

	// The regression guard: this change must not alter the ordinary stop path.
	t.Run("a canceled context still ends the block the way it did before", func(t *testing.T) {
		wait := func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
		if err := runBlock(t, wait, 50*time.Millisecond); err != nil {
			t.Fatalf("block = %v, want nil when the run is canceled", err)
		}
	})
}

// With no runner and no seam the Wait arm must remove itself rather than fire,
// so block still parks on the context exactly as it always has.
func TestGuestGoneIsNilWithoutARunner(t *testing.T) {
	h := &Host{}
	if ch := h.guestGone(context.Background()); ch != nil {
		t.Fatal("guestGone returned a live channel with no runner; the select arm would fire at once")
	}
}
