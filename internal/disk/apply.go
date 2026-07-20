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
		// A disk is an explicit pin, not the #155 auto-fallback default: honor the
		// disk's hosted choice verbatim (no silent Debian fallback, no strict-
		// sidecar override inherited from config.Default).
		cfg.HostedImageFallback = false

	case m.Image.Path != "":
		cfg.BaseImagePath = m.Image.Path
		cfg.HostedImageFallback = false

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
		cfg.HostedImageFallback = false

	default:
		return fmt.Errorf("disk %q has no resolvable image source", m.Name)
	}

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

	return nil
}
