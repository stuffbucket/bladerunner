//go:build darwin

package main

import "testing"

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
