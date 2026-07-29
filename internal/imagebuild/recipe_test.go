package imagebuild

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/provision"
)

func TestDefaultRecipeInstallsTheGuestStack(t *testing.T) {
	r := DefaultRecipe(BuildVersion(time.Date(2026, time.July, 29, 0, 0, 0, 0, time.UTC)))

	// The daemon, its client, the vsock relay tool, the JSON shim the guest
	// scripts use, sshd, and the time-heal daemon.
	for _, pkg := range []string{"incus", "incus-client", "socat", "jq", "openssh-server", "chrony"} {
		if !contains(r.Packages, pkg) {
			t.Errorf("Packages missing %q: %v", pkg, r.Packages)
		}
	}

	// incus-ui-canonical is NOT in Debian main, and apt-installing the Zabbly
	// build would swap Debian's incus to satisfy its "Depends: incus".
	if contains(r.Packages, "incus-ui-canonical") {
		t.Error("Packages must not contain incus-ui-canonical; it would replace Debian's incus")
	}

	if dup := firstDuplicate(r.Packages); dup != "" {
		t.Errorf("duplicate package %q", dup)
	}
}

// Without these two modules in the initramfs the guest cannot bring up vsock,
// and every relay the host depends on fails at boot.
func TestDefaultRecipeBakesVsockIntoTheInitramfs(t *testing.T) {
	r := DefaultRecipe("2026.07.29")
	for _, mod := range []string{"vmw_vsock_virtio_transport", "vhost_vsock"} {
		if !contains(r.InitramfsModules, mod) {
			t.Errorf("InitramfsModules missing %q: %v", mod, r.InitramfsModules)
		}
	}
}

// The recipe must take assets from internal/provision rather than restating
// them, so the image build and cloud-init cannot drift apart.
func TestDefaultRecipeTakesAssetsFromProvision(t *testing.T) {
	r := DefaultRecipe("2026.07.29")
	want := provision.ImageBuildAssets()

	if len(r.Assets) != len(want) {
		t.Fatalf("Assets length = %d, want %d (provision owns this set)", len(r.Assets), len(want))
	}
	for i, a := range want {
		if r.Assets[i].GuestPath != a.GuestPath || r.Assets[i].Content != a.Content {
			t.Errorf("asset %d = %q, want %q from provision", i, r.Assets[i].GuestPath, a.GuestPath)
		}
	}
}

// The version file location is owned by internal/config.
func TestDefaultRecipeStampsTheVersionFileFromConfig(t *testing.T) {
	const version = "2026.07.29"
	r := DefaultRecipe(version)

	if r.VersionPath != config.GuestImageVersionPath {
		t.Errorf("VersionPath = %q, want config.GuestImageVersionPath (%q)", r.VersionPath, config.GuestImageVersionPath)
	}
	if r.Version != version {
		t.Errorf("Version = %q, want %q", r.Version, version)
	}
}

func TestBuildVersionIsSortableAndDated(t *testing.T) {
	got := BuildVersion(time.Date(2026, time.July, 9, 13, 45, 0, 0, time.UTC))
	if got != "2026.07.09" {
		t.Errorf("BuildVersion() = %q, want %q (zero-padded so it sorts lexically)", got, "2026.07.09")
	}
}

// The watchdog unit is installed by the assets, so the recipe must also enable
// it; installing a unit without enabling it silently drops wake-heal.
func TestDefaultRecipeEnablesWhatItInstalls(t *testing.T) {
	r := DefaultRecipe("2026.07.29")
	for _, unit := range []string{"incus", "incus.socket", "ssh", "chrony", "bladerunner-watchdog.service"} {
		if !contains(r.EnableUnits, unit) {
			t.Errorf("EnableUnits missing %q: %v", unit, r.EnableUnits)
		}
	}
}

func contains(haystack []string, needle string) bool {
	return slices.Contains(haystack, needle)
}

func firstDuplicate(values []string) string {
	seen := make(map[string]bool, len(values))
	for _, v := range values {
		if seen[strings.ToLower(v)] {
			return v
		}
		seen[strings.ToLower(v)] = true
	}
	return ""
}
