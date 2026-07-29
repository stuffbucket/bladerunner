package imagebuild

import (
	"time"

	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/provision"
)

// buildVersionLayout formats the image version as a zero-padded, lexically
// sortable date. It matches the `date -u +%Y.%m.%d` stamp the shell build used,
// so versions stay comparable across the cutover.
const buildVersionLayout = "2006.01.02"

// Recipe describes what goes into the pre-baked guest image. It is plain data
// so that every mechanic — native chroot, libguestfs appliance, or VM — applies
// exactly the same changes, and so a test can assert on the recipe without
// building an image.
type Recipe struct {
	// Packages are installed from the distribution's own archive.
	Packages []string
	// EnableUnits are systemd units enabled after installation.
	EnableUnits []string
	// InitramfsModules are appended to /etc/initramfs-tools/modules before the
	// initramfs is regenerated.
	InitramfsModules []string
	// Assets are provisioning files copied into the guest root. Owned by
	// internal/provision, not restated here.
	Assets []provision.GuestAsset
	// VersionPath is where the build stamp is written in the guest. Owned by
	// internal/config.
	VersionPath string
	// Version is the build stamp itself.
	Version string
}

// BuildVersion renders t as the image build stamp.
func BuildVersion(t time.Time) string {
	return t.UTC().Format(buildVersionLayout)
}

// DefaultRecipe returns the recipe for the pre-baked bladerunner guest image,
// stamped with version.
//
// The package set is deliberately confined to Debian main. incus-ui-canonical is
// absent: it is not in main, and apt-installing the Zabbly build would swap
// Debian's incus to satisfy its "Depends: incus". The web UI is layered on
// separately as extracted static files, which is why it is not a package here.
func DefaultRecipe(version string) Recipe {
	return Recipe{
		Packages: []string{
			"incus",
			"incus-client",
			"socat",
			"jq",
			"openssh-server",
			"chrony",
		},
		// bladerunner-watchdog.service is enabled here because its unit arrives
		// via Assets: installing a unit without enabling it would silently drop
		// wake-heal on every baked image.
		EnableUnits: []string{
			"incus",
			"incus.socket",
			"ssh",
			"chrony",
			"bladerunner-watchdog.service",
		},
		// Without these the guest cannot bring up vsock, and every relay the
		// host depends on fails at boot.
		InitramfsModules: []string{
			"vmw_vsock_virtio_transport",
			"vhost_vsock",
		},
		Assets:      provision.ImageBuildAssets(),
		VersionPath: config.GuestImageVersionPath,
		Version:     version,
	}
}
