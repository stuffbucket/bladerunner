package main

import (
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/imagebuild"
)

// The Go policy layer names mechanics by what they are; the build script names
// them by the tool it reaches for. This mapping is the seam between them, and it
// must stay total: an unmapped method would otherwise reach the script as an
// empty --method and be silently reinterpreted as the script's own default.
func TestScriptMethodForMapsEveryBuildableMethod(t *testing.T) {
	tests := []struct {
		method imagebuild.Method
		want   string
	}{
		{imagebuild.MethodNative, "nbd"},
		{imagebuild.MethodAppliance, "guestfish"},
	}

	for _, tt := range tests {
		t.Run(string(tt.method), func(t *testing.T) {
			got, err := scriptMethodFor(tt.method)
			if err != nil {
				t.Fatalf("scriptMethodFor(%q) error = %v", tt.method, err)
			}
			if got != tt.want {
				t.Errorf("scriptMethodFor(%q) = %q, want %q", tt.method, got, tt.want)
			}
		})
	}
}

// The VM mechanic has no script equivalent. Until it exists, macOS must get a
// clear explanation rather than today's behavior, where the bake reaches the
// script and dies at modprobe with no hint that the platform is the problem.
func TestScriptMethodForRefusesTheVMMechanic(t *testing.T) {
	_, err := scriptMethodFor(imagebuild.MethodVM)
	if err == nil {
		t.Fatal("scriptMethodFor(vm) error = nil, want a clear unsupported error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "macos") {
		t.Errorf("error = %q, want it to explain the macOS situation", err)
	}
}

func TestScriptMethodForRejectsAnUnknownMethod(t *testing.T) {
	if _, err := scriptMethodFor(imagebuild.Method("banana")); err == nil {
		t.Fatal("scriptMethodFor() error = nil, want an error for an unmapped method")
	}
}
