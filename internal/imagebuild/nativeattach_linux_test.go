//go:build linux

package imagebuild

import (
	"os"
	"path/filepath"
	"testing"
)

// The native mechanic is executed by the build script's qemu-nbd path. Probing
// /dev/loop-control instead was wrong in both directions, and this project's own
// benchmarking demonstrated the gap: a container where loop-control was present
// could not load the nbd module at all, so the probe would accept a host the
// build then fails on. Reported as point 2 of #239.
func TestNativeAttachChecksWhatTheMechanicActuallyUses(t *testing.T) {
	dir := t.TempDir()
	nbd := filepath.Join(dir, "nbd0")
	if err := os.WriteFile(nbd, nil, 0o600); err != nil {
		t.Fatalf("create stand-in nbd node: %v", err)
	}

	tests := []struct {
		name     string
		nbdPath  string
		haveTool bool
		want     bool
	}{
		{"device and tool both present", nbd, true, true},
		{"device present but qemu-nbd missing", nbd, false, false},
		{"qemu-nbd present but no nbd device", filepath.Join(dir, "absent"), true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restorePath := nbdDevicePath
			restoreTool := haveQemuNBD
			nbdDevicePath = tt.nbdPath
			haveQemuNBD = func() bool { return tt.haveTool }
			t.Cleanup(func() { nbdDevicePath = restorePath; haveQemuNBD = restoreTool })

			if got := nativeAttachAvailable(); got != tt.want {
				t.Errorf("nativeAttachAvailable() = %v, want %v", got, tt.want)
			}
		})
	}
}
