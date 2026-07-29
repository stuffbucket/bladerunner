package imagebuild

// libguestfs invocation settings.
//
// These live in an untagged file, rather than beside the Linux-only probe that
// uses them, so their tests run on every platform. Both encode a diagnosed
// failure that is otherwise invisible until an aarch64 host tries to boot the
// appliance, and a test that only runs on Linux would not catch their removal
// during a refactor on a macOS workstation.
//
// The probe's tool name and timeout deliberately stay in probe_linux.go: they
// are mechanics of running it, not knowledge about making it work.
const (
	// libguestfsBackendEnv selects the hypervisor backend.
	libguestfsBackendEnv = "LIBGUESTFS_BACKEND"
	// libguestfsBackendDirect uses plain qemu instead of libvirt, which is not
	// necessarily configured on a build host and adds a second failure surface
	// for no benefit here.
	libguestfsBackendDirect = "direct"

	// libguestfsSettingsEnv carries backend tuning.
	libguestfsSettingsEnv = "LIBGUESTFS_BACKEND_SETTINGS"
	// libguestfsForceTCG makes libguestfs pick TCG explicitly.
	//
	// Without it, libguestfs on aarch64 misparses its own QMP capability probe,
	// concludes KVM is enabled, and emits KVM-only qemu flags (gic-version=host,
	// -cpu host). qemu then falls back to TCG and rejects those flags, so the
	// appliance never boots. Forcing TCG makes the probe and the real run agree.
	libguestfsForceTCG = "force_tcg"
)

// applianceEnv returns the environment for a libguestfs invocation: base with
// the settings above applied.
//
// The defaults are placed BEFORE base so that a value already present in base
// wins, since a process resolves duplicate environment entries last-first. That
// ordering is deliberate: an operator debugging one awkward host can export
// LIBGUESTFS_BACKEND themselves and have it take effect without editing the
// binary.
func applianceEnv(base []string) []string {
	env := make([]string, 0, len(base)+2)
	env = append(env,
		libguestfsBackendEnv+"="+libguestfsBackendDirect,
		libguestfsSettingsEnv+"="+libguestfsForceTCG,
	)
	return append(env, base...)
}
