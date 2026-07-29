package provision

import "io/fs"

// Guest-side destinations for the provisioning assets embedded in embed.go.
//
// These are the SINGLE SOURCE OF TRUTH for where each asset lands in the guest.
// Two paths install them — the cloud-init bootstrap (renderTimeHeal) and the
// pre-baked image build (internal/imagebuild) — and before these constants
// existed each restated the destinations as its own string literals, in Go and
// again in shell. docs/guest-wake-heal.md names that duplication as a drift
// hazard; referencing these constants is what closes it.
const (
	// GuestChronyConfPath is the suspend-tuned chrony config. It replaces the
	// package default, so it is a file overwrite rather than a drop-in.
	GuestChronyConfPath = "/etc/chrony/chrony.conf"
	// GuestWatchdogScriptPath is the guest-local wake-heal watchdog body.
	GuestWatchdogScriptPath = "/usr/local/sbin/bladerunner-watchdog.sh"
	// GuestWatchdogUnitPath is the systemd unit that supervises the watchdog.
	GuestWatchdogUnitPath = "/etc/systemd/system/bladerunner-watchdog.service"
)

// Modes for the assets above. The watchdog script is executed by systemd and so
// must carry the executable bits; the other two are read by daemons only.
const (
	chronyConfMode     fs.FileMode = 0o644
	watchdogScriptMode fs.FileMode = 0o755
	watchdogUnitMode   fs.FileMode = 0o644
)

// GuestAsset is one provisioning file to install into a guest root: its
// contents, where it goes, and the mode it needs once there.
type GuestAsset struct {
	// GuestPath is the absolute destination inside the guest filesystem.
	GuestPath string
	// Mode is the file mode to apply at the destination.
	Mode fs.FileMode
	// Content is the file body, embedded from internal/provision/scripts/.
	Content string
}

// ImageBuildAssets returns the provisioning files that the pre-baked image build
// installs into the guest root, so the time-heal stack (chrony + wake-heal
// watchdog) is present before first boot rather than being installed by
// cloud-init on every boot.
//
// The vsock relays are deliberately absent. After #160 every boot provisions via
// full cloud-init, which installs the templated bladerunner-vsock-relay@ unit
// and its per-channel arg files fresh each time; baking them would only create a
// stale duplicate that the cloud-init run has to supersede.
func ImageBuildAssets() []GuestAsset {
	return []GuestAsset{
		{GuestPath: GuestChronyConfPath, Mode: chronyConfMode, Content: chronyConf},
		{GuestPath: GuestWatchdogScriptPath, Mode: watchdogScriptMode, Content: watchdogScript},
		{GuestPath: GuestWatchdogUnitPath, Mode: watchdogUnitMode, Content: watchdogUnit},
	}
}
