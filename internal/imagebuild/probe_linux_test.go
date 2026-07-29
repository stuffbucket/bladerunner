//go:build linux

package imagebuild

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// withLoopControl points the probe at a path this test controls, so both
// outcomes are exercised regardless of the kernel the tests run under. Without
// this seam the true branch never executes: the CI container runs as root but
// has no /dev/loop-control, so the native-viable path — the one the whole
// "prefer native when it will actually work" policy turns on — went untested.
func withLoopControl(t *testing.T, path string) {
	t.Helper()
	original := loopControlPath
	loopControlPath = path
	t.Cleanup(func() { loopControlPath = original })
}

func TestLoopDeviceAvailableFollowsTheControlNode(t *testing.T) {
	present := filepath.Join(t.TempDir(), "loop-control")
	if err := os.WriteFile(present, nil, 0o600); err != nil {
		t.Fatalf("create stand-in control node: %v", err)
	}

	tests := []struct {
		name string
		path string
		want bool
	}{
		{"control node present", present, true},
		{"control node absent", filepath.Join(t.TempDir(), "absent"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withLoopControl(t, tt.path)
			if got := loopDeviceAvailable(); got != tt.want {
				t.Errorf("loopDeviceAvailable() = %v, want %v", got, tt.want)
			}
		})
	}
}

// The end-to-end selection on a host that CAN build natively: probe reports the
// capability, policy picks native, and the expensive appliance check is skipped.
func TestProbeSelectsNativeWhenTheHostCanDoIt(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root; the native mechanic mounts and chroots")
	}

	present := filepath.Join(t.TempDir(), "loop-control")
	if err := os.WriteFile(present, nil, 0o600); err != nil {
		t.Fatalf("create stand-in control node: %v", err)
	}
	withLoopControl(t, present)

	caps := Probe(context.Background(), MethodAuto, runtime.GOARCH)
	if !caps.LoopDevice {
		t.Fatal("LoopDevice = false, want true with the control node present")
	}
	if caps.ApplianceUsable {
		t.Error("ApplianceUsable = true, want the expensive check skipped when native is viable")
	}

	sel, err := Select(MethodAuto, runtime.GOARCH, caps)
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if sel.Method != MethodNative {
		t.Errorf("Method = %q, want %q on a host that can build natively", sel.Method, MethodNative)
	}
	if len(sel.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none when the fast path is taken", sel.Warnings)
	}
}

// Cross-architecture must block native even on an otherwise perfect host,
// because chroot cannot execute foreign binaries.
func TestProbeRefusesNativeForAForeignArchitecture(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to make the other native conditions hold")
	}

	present := filepath.Join(t.TempDir(), "loop-control")
	if err := os.WriteFile(present, nil, 0o600); err != nil {
		t.Fatalf("create stand-in control node: %v", err)
	}
	withLoopControl(t, present)

	foreign := "amd64"
	if runtime.GOARCH == foreign {
		foreign = "arm64"
	}

	caps := Probe(context.Background(), MethodAuto, foreign)
	if blockers := nativeBlockers(foreign, caps); len(blockers) == 0 {
		t.Fatalf("nativeBlockers(%q) is empty on a %s host, want the architecture mismatch reported", foreign, runtime.GOARCH)
	}
}
