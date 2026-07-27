package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stuffbucket/bladerunner/internal/control"
)

func TestDrainBudget(t *testing.T) {
	tests := []struct {
		name    string
		seconds int
		want    time.Duration
	}{
		{name: "explicit timeout is honored", seconds: 90, want: 90 * time.Second},
		{name: "zero falls back to the default", seconds: 0, want: control.DefaultEjectTimeoutSeconds * time.Second},
		{name: "negative falls back to the default", seconds: -5, want: control.DefaultEjectTimeoutSeconds * time.Second},
		{name: "oversized budget is capped", seconds: 3600, want: maxDrainRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := drainBudget(tt.seconds); got != tt.want {
				t.Errorf("drainBudget(%d) = %s, want %s", tt.seconds, got, tt.want)
			}
		})
	}
}

func TestWaitForStop(t *testing.T) {
	newSocket := func(t *testing.T) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "control.sock")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("create socket stand-in: %v", err)
		}
		return path
	}

	t.Run("reports stopped once the socket disappears", func(t *testing.T) {
		path := newSocket(t)
		reqErr := make(chan error, 1)
		go func() {
			time.Sleep(20 * time.Millisecond)
			_ = os.Remove(path)
			reqErr <- nil
		}()

		stopped, err := waitForStop(path, 2*time.Second, reqErr)
		if !stopped || err != nil {
			t.Fatalf("waitForStop = (%v, %v), want (true, nil)", stopped, err)
		}
	})

	t.Run("returns the request error instead of burning the budget", func(t *testing.T) {
		path := newSocket(t)
		reqErr := make(chan error, 1)
		want := errors.New("VM is not started yet")
		reqErr <- want

		start := time.Now()
		stopped, err := waitForStop(path, 10*time.Second, reqErr)
		if stopped {
			t.Fatal("waitForStop reported stopped while the socket still exists")
		}
		if !errors.Is(err, want) {
			t.Fatalf("err = %v, want %v", err, want)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("waited %s for a failed request; should return promptly", elapsed)
		}
	})

	t.Run("keeps waiting after a successful request", func(t *testing.T) {
		path := newSocket(t)
		reqErr := make(chan error, 1)
		reqErr <- nil

		stopped, err := waitForStop(path, 100*time.Millisecond, reqErr)
		if stopped {
			t.Fatal("waitForStop reported stopped while the socket still exists")
		}
		if err != nil {
			t.Fatalf("err = %v, want nil (plain timeout)", err)
		}
	})

	t.Run("succeeds when the socket is already gone", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "absent.sock")
		stopped, err := waitForStop(path, time.Second, make(chan error, 1))
		if !stopped || err != nil {
			t.Fatalf("waitForStop = (%v, %v), want (true, nil)", stopped, err)
		}
	})
}
