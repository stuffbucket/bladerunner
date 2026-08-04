package imagebuild

import (
	"errors"
	"strings"
	"testing"
)

// linuxCapable is a host that can bake: Linux, root, a usable nbd device, and
// building for its own architecture.
func linuxCapable() Capabilities {
	return Capabilities{
		GOOS:         "linux",
		HostArch:     "arm64",
		Elevated:     true,
		NativeAttach: true,
	}
}

// A capable host must be accepted.
//
// This is the assertion mutation testing showed was missing last time: every
// other test here asserts a REFUSAL, so inverting the check would leave them all
// green while no host on earth could bake. The success case has to be pinned
// separately from the failures.
func TestCheckHostAcceptsAHostThatCanBake(t *testing.T) {
	if err := CheckHost("arm64", linuxCapable()); err != nil {
		t.Fatalf("a capable host was refused: %v", err)
	}
}

// A refusal must name the specific blocking condition. "cannot build here" with
// no reason leaves an operator guessing at which of four things is wrong.
func TestCheckHostNamesTheBlockingCondition(t *testing.T) {
	tests := []struct {
		name       string
		targetArch string
		caps       Capabilities
		wantReason string
	}{
		{
			name:       "not root",
			targetArch: "arm64",
			caps: Capabilities{
				GOOS: "linux", HostArch: "arm64", Elevated: false, NativeAttach: true,
			},
			wantReason: "needs root",
		},
		{
			name:       "no block device",
			targetArch: "arm64",
			caps: Capabilities{
				GOOS: "linux", HostArch: "arm64", Elevated: true, NativeAttach: false,
			},
			wantReason: "cannot attach the image as a block device",
		},
		{
			name:       "cross-architecture",
			targetArch: "amd64",
			caps: Capabilities{
				GOOS: "linux", HostArch: "arm64", Elevated: true, NativeAttach: true,
			},
			wantReason: "cross-architecture",
		},
		{
			name:       "not linux",
			targetArch: "arm64",
			caps: Capabilities{
				GOOS: "darwin", HostArch: "arm64", Elevated: true, NativeAttach: false,
			},
			wantReason: "needs a Linux host",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckHost(tt.targetArch, tt.caps)
			if err == nil {
				t.Fatal("an unusable host was accepted")
			}
			if !strings.Contains(err.Error(), tt.wantReason) {
				t.Errorf("error %q does not name %q", err, tt.wantReason)
			}
			if !errors.Is(err, ErrUnsupportedHost) {
				t.Errorf("error %q does not wrap ErrUnsupportedHost", err)
			}
		})
	}
}

// Every blocking condition must be reported at once.
//
// Reporting one at a time makes an operator fix, retry, and discover the next —
// three round trips through a command that otherwise takes minutes to fail.
func TestCheckHostReportsEveryBlockerTogether(t *testing.T) {
	err := CheckHost("amd64", Capabilities{
		GOOS: "linux", HostArch: "arm64", Elevated: false, NativeAttach: false,
	})
	if err == nil {
		t.Fatal("a host blocked three ways was accepted")
	}

	for _, want := range []string{
		"needs root",
		"cannot attach the image as a block device",
		"cross-architecture",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%s", want, err)
		}
	}
}

// Off Linux the platform must be the ONLY blocker reported.
//
// Every other condition is true there too, but as a consequence rather than a
// cause. Telling a Mac user they need root invites them to try sudo, which
// cannot work — nothing about macOS is fixed by elevating.
func TestCheckHostBlamesOnlyThePlatformOffLinux(t *testing.T) {
	err := CheckHost("amd64", Capabilities{
		GOOS: "darwin", HostArch: "arm64", Elevated: false, NativeAttach: false,
	})
	if err == nil {
		t.Fatal("macOS was accepted as a build host")
	}

	for _, unwanted := range []string{"needs root", "qemu-nbd", "cross-architecture"} {
		if strings.Contains(err.Error(), unwanted) {
			t.Errorf("error offers %q as a fix on macOS, where it cannot help:\n%s", unwanted, err)
		}
	}
	if !strings.Contains(err.Error(), "needs a Linux host") {
		t.Errorf("error does not name the platform as the blocker:\n%s", err)
	}
}

// The refusal must say what to do instead.
//
// Most readers of this error are on a Mac, where the answer is a Linux VM they
// probably already have. An error that only says "no" sends them looking for a
// bug in bladerunner.
func TestCheckHostNamesTheWayOut(t *testing.T) {
	err := CheckHost("arm64", Capabilities{GOOS: "darwin", HostArch: "arm64"})
	if err == nil {
		t.Fatal("macOS was accepted as a build host")
	}
	for _, want := range []string{"Linux", "guest-image-latest"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not point at %q:\n%s", want, err)
		}
	}
}
