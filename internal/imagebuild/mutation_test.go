package imagebuild

import (
	"strings"
	"testing"
)

// Mutation testing found these. Each corresponds to a mutant that survived the
// original suite: a change to the production code that no test noticed.

// Every explicit-method test asserted a REFUSAL, so inverting the blocker check
// so that explicit methods always fail left the suite green. The success case
// needs asserting too, or "--method native" could stop working entirely without
// a single test noticing.
func TestSelectHonoursAnExplicitMethodThatIsViable(t *testing.T) {
	tests := []struct {
		name string
		want Method
		caps Capabilities
	}{
		{"native on a capable Linux host", MethodNative, linuxCapable()},
		{"appliance where libguestfs works", MethodAppliance, linuxCapable()},
		{"vm on macOS", MethodVM, Capabilities{GOOS: "darwin", HostArch: "arm64", VMUsable: true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Select(tt.want, "arm64", tt.caps)
			if err != nil {
				t.Fatalf("Select(%q) error = %v, want the request honored", tt.want, err)
			}
			if got.Method != tt.want {
				t.Errorf("Method = %q, want the requested %q", got.Method, tt.want)
			}
		})
	}
}

// vmBlockers reported on the VM runtime only after a redundant second GOOS
// test, so negating that test changed nothing any assertion observed. The two
// blocking conditions are now asserted apart.
func TestVMBlockersDistinguishesPlatformFromRuntime(t *testing.T) {
	tests := []struct {
		name        string
		caps        Capabilities
		wantBlocked bool
		wantReason  string
	}{
		{"macOS with a usable runtime", Capabilities{GOOS: "darwin", VMUsable: true}, false, ""},
		{"macOS without a usable runtime", Capabilities{GOOS: "darwin"}, true, "not implemented yet"},
		{"Linux, where the VM is not the mechanic", Capabilities{GOOS: "linux", VMUsable: true}, true, "macOS"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := vmBlockers(tt.caps)
			if tt.wantBlocked && len(got) == 0 {
				t.Fatalf("vmBlockers() is empty, want a blocker mentioning %q", tt.wantReason)
			}
			if !tt.wantBlocked && len(got) != 0 {
				t.Fatalf("vmBlockers() = %v, want none", got)
			}
			if tt.wantBlocked && !containsSubstring(got, tt.wantReason) {
				t.Errorf("vmBlockers() = %v, want one naming %q", got, tt.wantReason)
			}
		})
	}
}

// Inside Probe this decision is invisible off Linux, because applianceUsable
// reports false there whichever branch is taken. Testing the predicate directly
// makes the "skip the expensive check" rule assertable on any platform.
func TestShouldProbeApplianceSkipsTheExpensiveCheckWhenNativeWins(t *testing.T) {
	tests := []struct {
		name       string
		want       Method
		targetArch string
		caps       Capabilities
		expect     bool
	}{
		{"native viable, auto", MethodAuto, "arm64", linuxCapable(), false},
		{"native viable but appliance explicitly asked for", MethodAppliance, "arm64", linuxCapable(), true},
		{"native blocked by missing root", MethodAuto, "arm64", func() Capabilities {
			c := linuxCapable()
			c.Elevated = false
			return c
		}(), true},
		{"native blocked by architecture", MethodAuto, "amd64", linuxCapable(), true},
		{"off Linux entirely", MethodAuto, "arm64", Capabilities{GOOS: "darwin", HostArch: "arm64", VMUsable: true}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldProbeAppliance(tt.want, tt.targetArch, tt.caps); got != tt.expect {
				t.Errorf("shouldProbeAppliance() = %v, want %v", got, tt.expect)
			}
		})
	}
}

// containsSubstring reports whether any entry contains want.
func containsSubstring(entries []string, want string) bool {
	for _, e := range entries {
		if strings.Contains(e, want) {
			return true
		}
	}
	return false
}
