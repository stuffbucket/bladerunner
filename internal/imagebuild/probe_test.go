package imagebuild

import (
	"os"
	"runtime"
	"testing"
)

func TestProbeReportsTheRealHost(t *testing.T) {
	caps := Probe(runtime.GOARCH)

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

// The mechanic mounts and chroots, which cannot work off Linux. The probe must
// say so rather than letting the host check discover it later.
func TestProbeNeverClaimsANativeAttachOffLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("meaningful only off Linux")
	}
	if caps := Probe(runtime.GOARCH); caps.NativeAttach {
		t.Errorf("NativeAttach = true on %s, want false", runtime.GOOS)
	}
}

// Probe must feed CheckHost for the real host without either of them panicking
// or disagreeing. This is the seam where a platform probe bug would hide.
func TestProbeFeedsTheHostCheck(t *testing.T) {
	caps := Probe(runtime.GOARCH)
	err := CheckHost(runtime.GOARCH, caps)

	// On an arbitrary developer or CI machine either answer is legitimate: a
	// Linux box with root and an nbd device can bake, and nothing else can. The
	// assertion is that the two agree about which.
	canBake := runtime.GOOS == "linux" && caps.Elevated && caps.NativeAttach
	if canBake && err != nil {
		t.Errorf("host looks capable but CheckHost refused: %v", err)
	}
	if !canBake && err == nil {
		t.Errorf("host cannot bake (GOOS=%s elevated=%v attach=%v) but CheckHost accepted it",
			runtime.GOOS, caps.Elevated, caps.NativeAttach)
	}
}
