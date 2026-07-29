package provision

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/config"
)

// The image build and the cloud-init path must install the same files to the
// same places. Before this, both restated the destinations as string literals —
// cloud-init in renderTimeHeal, the image build in scripts/build-guest-image.sh —
// which is exactly the drift docs/guest-wake-heal.md warns about.
func TestImageBuildAssetsCoverTheTimeHealStack(t *testing.T) {
	assets := ImageBuildAssets()
	if len(assets) == 0 {
		t.Fatal("ImageBuildAssets() is empty")
	}

	byPath := make(map[string]GuestAsset, len(assets))
	for _, a := range assets {
		if _, dup := byPath[a.GuestPath]; dup {
			t.Errorf("duplicate destination %q", a.GuestPath)
		}
		if a.Content == "" {
			t.Errorf("asset %q has empty content", a.GuestPath)
		}
		if !strings.HasPrefix(a.GuestPath, "/") {
			t.Errorf("asset %q must be an absolute guest path", a.GuestPath)
		}
		byPath[a.GuestPath] = a
	}

	tests := []struct {
		path string
		mode fs.FileMode
		want string
	}{
		{GuestChronyConfPath, chronyConfMode, chronyConf},
		{GuestWatchdogScriptPath, watchdogScriptMode, watchdogScript},
		{GuestWatchdogUnitPath, watchdogUnitMode, watchdogUnit},
	}
	for _, tt := range tests {
		got, ok := byPath[tt.path]
		if !ok {
			t.Errorf("no asset for %q", tt.path)
			continue
		}
		if got.Content != tt.want {
			t.Errorf("asset %q content differs from the embedded file", tt.path)
		}
		if got.Mode != tt.mode {
			t.Errorf("asset %q mode = %v, want %v", tt.path, got.Mode, tt.mode)
		}
	}
}

// The watchdog script is executed by systemd, so it must stay executable
// wherever it is installed from.
func TestWatchdogScriptIsExecutable(t *testing.T) {
	for _, a := range ImageBuildAssets() {
		if a.GuestPath != GuestWatchdogScriptPath {
			continue
		}
		if a.Mode&0o111 == 0 {
			t.Fatalf("watchdog script mode = %v, want the executable bits set", a.Mode)
		}
		return
	}
	t.Fatalf("no asset for %q", GuestWatchdogScriptPath)
}

// The cloud-init path must write to the same constants, so that changing a
// destination in one place cannot silently diverge from the other.
func TestCloudInitUsesTheSharedDestinations(t *testing.T) {
	cfg := testConfig()
	fragment := renderTimeHeal(cfg)

	for _, path := range []string{GuestChronyConfPath, GuestWatchdogScriptPath, GuestWatchdogUnitPath} {
		if !strings.Contains(fragment, path) {
			t.Errorf("cloud-init fragment does not mention %q", path)
		}
	}
}

// The version file the build stamps is owned by internal/config, not restated
// here.
func TestImageVersionPathComesFromConfig(t *testing.T) {
	if config.GuestImageVersionPath == "" {
		t.Fatal("config.GuestImageVersionPath is empty")
	}
	if !strings.HasPrefix(config.GuestImageVersionPath, "/") {
		t.Errorf("GuestImageVersionPath = %q, want an absolute guest path", config.GuestImageVersionPath)
	}
}
