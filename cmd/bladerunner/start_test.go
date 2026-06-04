package main

import (
	"testing"
	"time"

	"github.com/stuffbucket/bladerunner/internal/control"
)

func TestEjectTimeoutFromArgs(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want time.Duration
	}{
		{"explicit seconds", "eject 45", 45 * time.Second},
		{"explicit with force", "eject 30 force", 30 * time.Second},
		{"absent uses default", "eject", control.DefaultEjectTimeoutSeconds * time.Second},
		{"zero uses default", "eject 0", control.DefaultEjectTimeoutSeconds * time.Second},
		{"garbage uses default", "eject abc", control.DefaultEjectTimeoutSeconds * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := control.NewRequest(tc.raw)
			if got := ejectTimeoutFromArgs(req); got != tc.want {
				t.Fatalf("ejectTimeoutFromArgs(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestEjectForceFromArgs(t *testing.T) {
	if !ejectForceFromArgs(control.NewRequest("eject 30 force")) {
		t.Error("force arg should be detected")
	}
	if ejectForceFromArgs(control.NewRequest("eject 30")) {
		t.Error("no force arg should be false")
	}
	if ejectForceFromArgs(control.NewRequest("eject")) {
		t.Error("absent args should be false")
	}
}
