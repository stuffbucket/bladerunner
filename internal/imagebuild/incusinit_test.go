package imagebuild

import (
	"strings"
	"testing"
)

// The recipe must initialize Incus at bake time.
//
// Doing it on first boot costs a measured 2m19s of a 192s first boot — the
// largest single component, and work whose result is identical on every
// machine.
func TestRecipeInitializesIncusAtBakeTime(t *testing.T) {
	var found bool
	for _, s := range DefaultRecipe(testVersion).Steps() {
		if strings.Contains(strings.Join(s.Argv, " "), "incus admin init --preseed") {
			found = true
			if s.Optional {
				t.Error("the Incus init is optional; it reaches no third party, so a " +
					"failure is a recipe bug and must fail the bake")
			}
		}
	}
	if !found {
		t.Error("no step runs `incus admin init --auto`; every first boot pays for it again")
	}
}

// The daemon must be invoked by its real path.
//
// There is no `incusd` in /usr/bin or /usr/sbin — the incus package ships it
// under /usr/libexec, and the path is only discoverable from the systemd unit's
// ExecStart. Naming it bare finds nothing and the init silently never runs.
func TestIncusInitUsesTheLibexecDaemonPath(t *testing.T) {
	script := incusInitScript()

	if !strings.Contains(script, incusDaemonPath) {
		t.Errorf("the init script does not invoke %s", incusDaemonPath)
	}
	for _, wrong := range []string{"\nincusd ", " incusd ", "/usr/bin/incusd", "/usr/sbin/incusd"} {
		if strings.Contains(script, wrong) {
			t.Errorf("the script invokes the daemon as %q, which does not exist on the guest", strings.TrimSpace(wrong))
		}
	}
}

// The server identity must NOT ship inside the image.
//
// `incus admin init` generates a server certificate and PRIVATE KEY. Baked and
// published, every VM built from the image would share them, and the key would
// be in a public release — so anyone could impersonate any user's Incus API.
// incusd regenerates a fresh pair on first boot when it finds none.
func TestIncusInitDoesNotShipTheServerKey(t *testing.T) {
	script := incusInitScript()

	for _, path := range []string{"/var/lib/incus/server.crt", "/var/lib/incus/server.key"} {
		if !strings.Contains(script, path) {
			t.Errorf("the script never removes %s; it would ship in a public image", path)
		}
	}
	if !strings.Contains(script, "rm -f") {
		t.Error("the script names the server identity but does not remove it")
	}
}

// The bake must fail if the daemon never comes up.
//
// Without `set -e` a failed init is followed by a successful cleanup, and the
// step reports success having done nothing — which is exactly the silent
// half-built image this package exists to prevent.
func TestIncusInitFailsLoudlyRatherThanSilently(t *testing.T) {
	script := incusInitScript()

	if !strings.HasPrefix(script, "set -e") {
		t.Error("the script does not start with `set -e`; a failed init would report success")
	}
	if !strings.Contains(script, "exit 1") {
		t.Error("the script does not fail when the daemon never creates its socket")
	}
}

// The bake must NOT choose the guest's network.
//
// `incus admin init --auto` picks the incusbr0 subnet by scanning the
// interfaces of the machine it runs on. Baked, that freezes a range chosen on a
// build runner into every user's guest, where it may collide with their own
// network — and the real bake proved the failure mode is not hypothetical: the
// builder had no free subnet and the step failed outright.
//
// The storage pool has no such dependency, which is why it is the half worth
// baking.
func TestIncusInitDoesNotBakeTheNetwork(t *testing.T) {
	script := incusInitScript()

	if strings.Contains(script, "--auto") {
		t.Error("the init uses --auto, which also creates incusbr0 with a " +
			"subnet chosen on the BUILD machine")
	}
	if strings.Contains(incusStoragePreseed, "networks:") {
		t.Error("the preseed declares networks; the guest's network must stay a first-boot decision")
	}
	if !strings.Contains(incusStoragePreseed, "storage_pools:") {
		t.Error("the preseed creates no storage pool, so it saves nothing")
	}
}
