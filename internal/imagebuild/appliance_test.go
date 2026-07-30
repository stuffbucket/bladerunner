package imagebuild

import (
	"slices"
	"strings"
	"testing"
)

// These two settings are the whole value of the libguestfs diagnosis, and both
// are invisible in a passing build until an aarch64 host tries to use the
// appliance. Asserting them here means deleting either one breaks a test rather
// than silently breaking ARM image builds.
func TestApplianceEnvCarriesTheSettingsThatMakeItBoot(t *testing.T) {
	env := ApplianceEnv(nil)

	tests := []struct {
		name string
		key  string
		want string
		why  string
	}{
		{
			name: "force TCG",
			key:  "LIBGUESTFS_BACKEND_SETTINGS",
			want: "force_tcg",
			why: "without it libguestfs on aarch64 misparses its own QMP probe, concludes KVM is " +
				"enabled, and emits gic-version=host and -cpu host; qemu then falls back to TCG " +
				"and rejects those flags, so the appliance never boots",
		},
		{
			name: "direct backend",
			key:  "LIBGUESTFS_BACKEND",
			want: "direct",
			why:  "libvirt is not necessarily configured on a build host and adds a second failure surface",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := lookupEnv(env, tt.key)
			if !ok {
				t.Fatalf("%s is not set; it is required because %s", tt.key, tt.why)
			}
			if got != tt.want {
				t.Errorf("%s = %q, want %q: %s", tt.key, got, tt.want, tt.why)
			}
		})
	}
}

// The probe inherits the caller's environment, so a host that needs a proxy or
// a custom TMPDIR to reach its tooling must keep them.
func TestApplianceEnvPreservesTheInheritedEnvironment(t *testing.T) {
	base := []string{"HTTPS_PROXY=http://proxy.example:3128", "TMPDIR=/var/tmp/custom"}

	env := ApplianceEnv(base)
	for _, want := range base {
		if !slices.Contains(env, want) {
			t.Errorf("ApplianceEnv() dropped %q from the inherited environment", want)
		}
	}
}

// A caller-supplied value must win, so an operator debugging a specific host can
// override the defaults without editing the binary.
func TestApplianceEnvLetsTheCallerOverride(t *testing.T) {
	env := ApplianceEnv([]string{"LIBGUESTFS_BACKEND=libvirt"})

	got, ok := lookupEnv(env, "LIBGUESTFS_BACKEND")
	if !ok {
		t.Fatal("LIBGUESTFS_BACKEND is not set")
	}
	if got != "libvirt" {
		t.Errorf("LIBGUESTFS_BACKEND = %q, want the caller's %q to win", got, "libvirt")
	}
}

// lookupEnv returns the last value for key, matching how a process resolves
// duplicate entries in its environment.
func lookupEnv(env []string, key string) (string, bool) {
	value, found := "", false
	for _, entry := range env {
		name, v, ok := strings.Cut(entry, "=")
		if ok && name == key {
			value, found = v, true
		}
	}
	return value, found
}
