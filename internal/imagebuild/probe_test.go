package imagebuild

import (
	"context"
	"os"
	"runtime"
	"testing"
)

func TestProbeReportsTheRealHost(t *testing.T) {
	caps := Probe(context.Background(), MethodAuto, runtime.GOARCH)

	if caps.GOOS != runtime.GOOS {
		t.Errorf("GOOS = %q, want %q", caps.GOOS, runtime.GOOS)
	}
	if caps.HostArch != runtime.GOARCH {
		t.Errorf("HostArch = %q, want %q", caps.HostArch, runtime.GOARCH)
	}
	if want := os.Geteuid() == 0; caps.Elevated != want {
		t.Errorf("Elevated = %v, want %v", caps.Elevated, want)
	}
}

// The native mechanic mounts and chroots, which cannot work off Linux. The probe
// must say so rather than letting policy discover it later.
func TestProbeNeverClaimsANativeAttachOffLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("meaningful only off Linux")
	}
	if caps := Probe(context.Background(), MethodAuto, runtime.GOARCH); caps.NativeAttach {
		t.Errorf("NativeAttach = true on %s, want false", runtime.GOOS)
	}
}

// Launching the libguestfs appliance costs seconds, so it must not be probed
// when the native path is already viable. This keeps the common case fast.
func TestProbeSkipsTheApplianceCheckWhenNativeIsViable(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("needs a Linux host running as root for native to be viable")
	}
	caps := Probe(context.Background(), MethodAuto, runtime.GOARCH)
	if !caps.NativeAttach {
		t.Skip("no loop device, so native is not viable here")
	}
	if caps.ApplianceUsable {
		t.Error("ApplianceUsable = true, want the expensive check skipped when native wins")
	}
}

// A canceled context must not leave the caller waiting on a microVM boot.
func TestProbeHonoursContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	caps := Probe(ctx, MethodAppliance, runtime.GOARCH)
	if caps.ApplianceUsable {
		t.Error("ApplianceUsable = true with a canceled context, want false")
	}
}

// Probe must select the same mechanic policy would, for the real host. This is
// the seam where a platform probe bug would otherwise hide.
func TestProbeFeedsPolicyWithoutPanicking(t *testing.T) {
	caps := Probe(context.Background(), MethodAuto, runtime.GOARCH)

	sel, err := Select(MethodAuto, runtime.GOARCH, caps)
	switch runtime.GOOS {
	case "linux", "darwin":
		// Either a method is chosen, or the error explains every blocker. Both
		// are acceptable on an arbitrary developer or CI machine.
		if err != nil && sel.Method != "" {
			t.Errorf("got both an error (%v) and a method (%q)", err, sel.Method)
		}
	default:
		if err == nil {
			t.Errorf("Select() error = nil on %s, want unsupported", runtime.GOOS)
		}
	}
}
