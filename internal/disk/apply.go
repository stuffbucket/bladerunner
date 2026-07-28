package disk

import (
	"fmt"
	"runtime"

	"github.com/stuffbucket/bladerunner/internal/config"
)

// ApplyTo maps a manifest onto cfg as manifest-derived DEFAULTS. It is called
// AFTER config.Default and BEFORE the cobra-flag overrides, so explicit flags
// (--cpus/--memory/--disk/--gui/--headless) win. Slot isolation (StateDir,
// VMDir, DiskPath, SavedStatePath, ...) is the caller's job via the baseDir
// passed to config.Default; ApplyTo never touches those fields.
//
// Numeric sizing is left at config.Default's values when a VM field is zero;
// the authoritative numeric check happens later in config.Validate().
func (m *Manifest) ApplyTo(cfg *config.Config) error {
	goarch := runtime.GOARCH
	if cfg.Arch != "" {
		goarch = cfg.Arch
	}

	switch {
	case m.Image.Hosted:
		// Re-resolve: config.Default hardcodes useHosted=false.
		url, err := config.HostedGuestImageURL(goarch)
		if err != nil {
			return fmt.Errorf("resolve hosted image url: %w", err)
		}
		cfg.UseHostedGuestImage = true
		cfg.BaseImageURL = url
		cfg.BaseImagePath = ""
		cfg.BaseImageSHA512 = ""         // hosted verified via sidecar
		cfg.BaseImageExpectedSHA256 = "" // sidecar fallback

	case m.Image.Path != "":
		cfg.BaseImagePath = m.Image.Path

	case len(m.Image.Arches) > 0:
		arch, ok := m.Image.Arches[goarch]
		if !ok {
			return fmt.Errorf("disk %q has no image for architecture %q", m.Name, goarch)
		}
		cfg.UseHostedGuestImage = false
		cfg.BaseImageURL = arch.URL
		cfg.BaseImagePath = ""
		cfg.BaseImageSHA512 = ""                  // not the pinned Debian default
		cfg.BaseImageExpectedSHA256 = arch.SHA256 // explicit expected digest

	default:
		return fmt.Errorf("disk %q has no resolvable image source", m.Name)
	}

	m.ApplyDefaultsTo(cfg)

	return nil
}

// ApplyDefaultsTo applies the parts of a manifest that are NOT the image
// source: the sizing recommendations and the boot mode. A zero sizing field
// leaves config.Default's value alone.
//
// It is split out of ApplyTo for one caller that must not resolve an image at
// all — a cartridge, which carries its own root.img and whose packed manifest
// is therefore consulted only for "how big should this VM be, and does it want
// a window". Resolving the image there would be worse than useless: the
// cartridge overwrites every image field a moment later, and a manifest that
// names an architecture this host does not have would fail a boot that was
// never going to download anything.
func (m *Manifest) ApplyDefaultsTo(cfg *config.Config) {
	// Sizing defaults only: 0 => leave config.Default's value.
	if m.VM.CPUs > 0 {
		cfg.CPUs = m.VM.CPUs
	}
	if m.VM.MemoryGiB > 0 {
		cfg.MemoryGiB = m.VM.MemoryGiB
	}
	if m.VM.DiskSizeGiB > 0 {
		cfg.DiskSizeGiB = m.VM.DiskSizeGiB
	}

	cfg.GUI = m.Boot.Mode == BootModeGUI
}
