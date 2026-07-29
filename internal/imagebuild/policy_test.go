package imagebuild

import (
	"errors"
	"strings"
	"testing"
)

// linuxCapable is a host that can take the fast native path: Linux, root, a
// usable loop device, and building for its own architecture.
func linuxCapable() Capabilities {
	return Capabilities{
		GOOS:            "linux",
		HostArch:        "arm64",
		Elevated:        true,
		LoopDevice:      true,
		ApplianceUsable: true,
	}
}

func TestSelectPrefersNativeWhenItWillActuallyWork(t *testing.T) {
	got, err := Select(MethodAuto, "arm64", linuxCapable())
	if err != nil {
		t.Fatalf("Select() error = %v, want nil", err)
	}
	if got.Method != MethodNative {
		t.Errorf("Method = %q, want %q", got.Method, MethodNative)
	}
	if len(got.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none when the fast path is taken", got.Warnings)
	}
}

// The fallback must name the specific blocking condition. A bare "falling back"
// with no reason is what this project's shell rule forbids.
func TestSelectFallsBackLoudlyWithASpecificReason(t *testing.T) {
	tests := []struct {
		name       string
		targetArch string
		caps       Capabilities
		wantMethod Method
		wantReason string
	}{
		{
			name:       "not root",
			targetArch: "arm64",
			caps: func() Capabilities {
				c := linuxCapable()
				c.Elevated = false
				return c
			}(),
			wantMethod: MethodAppliance,
			wantReason: "root",
		},
		{
			name:       "no loop device",
			targetArch: "arm64",
			caps: func() Capabilities {
				c := linuxCapable()
				c.LoopDevice = false
				return c
			}(),
			wantMethod: MethodAppliance,
			wantReason: "loop device",
		},
		{
			name:       "cross architecture",
			targetArch: "amd64",
			caps:       linuxCapable(),
			wantMethod: MethodAppliance,
			wantReason: "architecture",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Select(MethodAuto, tt.targetArch, tt.caps)
			if err != nil {
				t.Fatalf("Select() error = %v, want nil", err)
			}
			if got.Method != tt.wantMethod {
				t.Errorf("Method = %q, want %q", got.Method, tt.wantMethod)
			}
			if len(got.Warnings) == 0 {
				t.Fatal("Warnings is empty; a fallback must say why it happened")
			}
			joined := strings.ToLower(strings.Join(got.Warnings, " "))
			if !strings.Contains(joined, tt.wantReason) {
				t.Errorf("Warnings = %v, want one naming %q", got.Warnings, tt.wantReason)
			}
		})
	}
}

// On macOS the VM is not a degraded path, it is the only and correct one: a
// macOS host can neither chroot into a Linux root nor run libguestfs. Warning
// about the absent native path on every build would be unfixable noise, so the
// selection must be clean.
func TestSelectOnDarwinChoosesTheVMWithoutWarning(t *testing.T) {
	got, err := Select(MethodAuto, "arm64", Capabilities{
		GOOS: "darwin", HostArch: "arm64", VMUsable: true,
	})
	if err != nil {
		t.Fatalf("Select() error = %v, want nil", err)
	}
	if got.Method != MethodVM {
		t.Errorf("Method = %q, want %q", got.Method, MethodVM)
	}
	if len(got.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none: the VM is the expected path on macOS, not a fallback", got.Warnings)
	}
}

// Windows has neither mechanic. The error must point at WSL2 rather than
// leaving the user to guess.
func TestSelectWindowsIsUnsupportedAndNamesWSL2(t *testing.T) {
	_, err := Select(MethodAuto, "amd64", Capabilities{GOOS: "windows", HostArch: "amd64"})
	if err == nil {
		t.Fatal("Select() error = nil, want an unsupported-platform error")
	}
	if !errors.Is(err, ErrUnsupportedPlatform) {
		t.Errorf("error = %v, want it to wrap ErrUnsupportedPlatform", err)
	}
	if !strings.Contains(strings.ToUpper(err.Error()), "WSL2") {
		t.Errorf("error = %q, want it to name WSL2", err)
	}
}

// An explicitly requested method must fail loudly when it cannot run. Silently
// substituting a different mechanic would hide the operator's mistake.
func TestSelectExplicitMethodDoesNotSilentlyFallBack(t *testing.T) {
	tests := []struct {
		name string
		want Method
		caps Capabilities
	}{
		{"native on darwin", MethodNative, Capabilities{GOOS: "darwin", HostArch: "arm64", VMUsable: true}},
		{"native without root", MethodNative, func() Capabilities {
			c := linuxCapable()
			c.Elevated = false
			return c
		}()},
		{"appliance that will not launch", MethodAppliance, func() Capabilities {
			c := linuxCapable()
			c.ApplianceUsable = false
			return c
		}()},
		{"vm on linux", MethodVM, linuxCapable()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Select(tt.want, "arm64", tt.caps); err == nil {
				t.Fatalf("Select(%q) error = nil, want a refusal rather than a silent fallback", tt.want)
			}
		})
	}
}

// With no mechanic left, the error must carry every blocker so the operator can
// fix them in one pass instead of one per run.
func TestSelectReportsAllBlockersWhenNothingIsUsable(t *testing.T) {
	caps := linuxCapable()
	caps.Elevated = false
	caps.ApplianceUsable = false

	_, err := Select(MethodAuto, "arm64", caps)
	if err == nil {
		t.Fatal("Select() error = nil, want an error when no method is usable")
	}
	msg := strings.ToLower(err.Error())
	for _, want := range []string{"root", "libguestfs"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

func TestSelectRejectsAnUnknownMethod(t *testing.T) {
	if _, err := Select(Method("banana"), "arm64", linuxCapable()); err == nil {
		t.Fatal("Select() error = nil, want an error for an unknown method")
	}
}
