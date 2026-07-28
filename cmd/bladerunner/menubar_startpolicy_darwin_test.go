//go:build darwin

package main

import (
	"testing"
	"time"

	"github.com/stuffbucket/bladerunner/internal/config"
)

func TestWebShellEnabled(t *testing.T) {
	tests := []struct {
		name        string
		st          vmState
		firstAction bool
		want        bool
	}{
		{"healthy always enabled", vmHealthy, false, true},
		{"healthy enabled under first-action", vmHealthy, true, true},
		{"stopped disabled by default", vmStopped, false, false},
		{"stopped enabled under first-action", vmStopped, true, true},
		{"wedged disabled", vmWedged, false, false},
		{"wedged disabled even under first-action", vmWedged, true, false},
		{"unknown disabled", vmUnknown, false, false},
		{"unknown disabled even under first-action", vmUnknown, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := webShellEnabled(tt.st, tt.firstAction); got != tt.want {
				t.Errorf("webShellEnabled(%v, %v) = %v, want %v", tt.st, tt.firstAction, got, tt.want)
			}
		})
	}
}

// TestEnablementFor pins the whole action-row policy, not just Web/Shell: a
// stopped VM offers only Start, a running one offers the three recovery
// actions, and StartOnFirstAction is the single exception that keeps Web/Shell
// clickable while stopped so a click can lazily boot.
func TestEnablementFor(t *testing.T) {
	tests := []struct {
		name        string
		st          vmState
		firstAction bool
		want        menuEnablement
	}{
		{
			name: "stopped offers only start",
			st:   vmStopped,
			want: menuEnablement{start: true},
		},
		{
			name:        "stopped under first-action also offers web and shell",
			st:          vmStopped,
			firstAction: true,
			want:        menuEnablement{start: true, web: true, shell: true},
		},
		{
			name: "healthy offers every running action",
			st:   vmHealthy,
			want: menuEnablement{
				stop: true, reconnect: true, restart: true, web: true, shell: true,
			},
		},
		{
			name: "wedged offers recovery but not web or shell",
			st:   vmWedged,
			want: menuEnablement{stop: true, reconnect: true, restart: true},
		},
		{
			name:        "wedged under first-action still withholds web and shell",
			st:          vmWedged,
			firstAction: true,
			want:        menuEnablement{stop: true, reconnect: true, restart: true},
		},
		{
			name: "unknown is treated like wedged",
			st:   vmUnknown,
			want: menuEnablement{stop: true, reconnect: true, restart: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := enablementFor(tt.st, tt.firstAction); got != tt.want {
				t.Errorf("enablementFor(%v, %v) = %+v, want %+v",
					tt.st, tt.firstAction, got, tt.want)
			}
		})
	}
}

// TestStatusTitle covers the status row, including the reason the boot phase
// exists: a guest that is not answering *yet* must not be labeled as broken.
func TestStatusTitle(t *testing.T) {
	tests := []struct {
		name    string
		st      vmState
		phase   string
		booting bool
		want    string
	}{
		{"stopped", vmStopped, "", false, "Stopped"},
		{"healthy", vmHealthy, "", false, "Running — healthy"},
		{"wedged with no boot underway", vmWedged, "", false, "Running — not responding"},
		{"unknown with no boot underway", vmUnknown, "", false, "Running — status unknown"},
		{
			name:    "a boot in progress wins over the wedge label",
			st:      vmWedged,
			phase:   "Starting Incus…",
			booting: true,
			want:    "Starting Incus…",
		},
		{
			name:    "a boot in progress wins over the unknown label",
			st:      vmUnknown,
			phase:   "Booting Linux…",
			booting: true,
			want:    "Booting Linux…",
		},
		{
			name: "a healthy guest ignores a stale boot phase",
			st:   vmHealthy, phase: "Booting Linux…", booting: true,
			want: "Running — healthy",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The flat default instance: its wording must stay exactly what
			// the single-VM install has always shown.
			target := menubarTarget{name: config.DefaultInstanceName, isDefault: true}
			if got := statusTitle(tt.st, tt.phase, tt.booting, target); got != tt.want {
				t.Errorf("statusTitle(%v, %q, %v) = %q, want %q",
					tt.st, tt.phase, tt.booting, got, tt.want)
			}
		})
	}
}

// TestHostWokeFromSleep checks the wall-clock jump that stands in for "the Mac
// slept". The threshold is one poll interval plus wakeGapSeconds, so an
// ordinary poll (and a slightly late one) must not be mistaken for a wake.
func TestHostWokeFromSleep(t *testing.T) {
	interval := int64(menubarRefreshInterval / time.Second)

	tests := []struct {
		name string
		st   vmState
		gap  int64
		want bool
	}{
		{"an ordinary poll is not a wake", vmHealthy, interval, false},
		{"a late poll inside the tolerance is not a wake", vmHealthy, interval + wakeGapSeconds, false},
		{"a jump past the tolerance is a wake", vmHealthy, interval + wakeGapSeconds + 1, true},
		{"a long sleep is a wake", vmHealthy, 3600, true},
		{"a wedged guest can still report a wake", vmWedged, 3600, true},
		{"an unknown guest can still report a wake", vmUnknown, 3600, true},
		{"a stopped VM never reports a wake", vmStopped, 3600, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const prevWall = 1_700_000_000
			got := hostWokeFromSleep(tt.st, prevWall, prevWall+tt.gap)
			if got != tt.want {
				t.Errorf("hostWokeFromSleep(%v, gap=%ds) = %v, want %v",
					tt.st, tt.gap, got, tt.want)
			}
		})
	}
}

// TestOfferLatestNeverBlocks is the safety property the poll loop depends on:
// publishing a reading must not stall when the menu goroutine has not drained
// the previous one, because the poll loop also drives wake detection and the
// notifier.
func TestOfferLatestNeverBlocks(t *testing.T) {
	ch := make(chan vmState, 1)

	offerLatest(ch, vmHealthy)
	// The slot is now full. A second offer must be dropped, not queued and not
	// blocked on.
	offerLatest(ch, vmWedged)

	select {
	case got := <-ch:
		if got != vmHealthy {
			t.Errorf("got %v, want the first offer %v to be kept", got, vmHealthy)
		}
	default:
		t.Fatal("the first offer was not delivered")
	}

	select {
	case got := <-ch:
		t.Fatalf("the dropped offer %v was queued after all", got)
	default:
	}
}
