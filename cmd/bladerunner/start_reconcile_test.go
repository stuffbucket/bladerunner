package main

import (
	"testing"
	"time"

	"github.com/stuffbucket/bladerunner/internal/config"
)

// changedSet returns a predicate reporting whether a flag name is in the set,
// standing in for cmd.Flags().Changed in tests.
func changedSet(names ...string) func(string) bool {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return func(name string) bool { return set[name] }
}

// withStartFlags sets the package-global startFlags for the duration of a test
// and restores it afterward, so tests don't leak flag state into one another.
func withStartFlags(t *testing.T, f func()) {
	t.Helper()
	saved := startFlags
	t.Cleanup(func() { startFlags = saved })
	f()
}

// On a plain `br start` with no flags changed, applyFlagOverrides must leave the
// persisted Settings baseline intact (nothing is clobbered by flag defaults).
func TestApplyFlagOverridesPlainNoChangeKeepsSettings(t *testing.T) {
	cfg, err := config.Default(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := config.DefaultSettings()
	s.CPUs = 8
	s.MemoryGiB = 16
	s.DiskSizeGiB = 128
	s.WaitForIncus = config.Duration(3 * time.Minute)
	s.ApplyTo(cfg)

	withStartFlags(t, func() {
		// startFlags holds cobra defaults the user didn't touch.
		startFlags.cpus = config.DefaultCPUs
		startFlags.memory = config.DefaultMemoryGiB
		startFlags.disk = config.DefaultDiskSizeGiB
		startFlags.timeout = config.DefaultTimeout
		applyFlagOverrides(cfg, changedSet(), false)
	})

	if cfg.CPUs != 8 {
		t.Errorf("CPUs = %d, want persisted 8", cfg.CPUs)
	}
	if cfg.MemoryGiB != 16 {
		t.Errorf("MemoryGiB = %d, want persisted 16", cfg.MemoryGiB)
	}
	if cfg.DiskSizeGiB != 128 {
		t.Errorf("DiskSizeGiB = %d, want persisted 128", cfg.DiskSizeGiB)
	}
	if cfg.WaitForIncus != 3*time.Minute {
		t.Errorf("WaitForIncus = %v, want persisted 3m", cfg.WaitForIncus)
	}
}

// A flag the user actually changed overrides the persisted Settings value, and
// only that field changes.
func TestApplyFlagOverridesPlainChangedWins(t *testing.T) {
	cfg, err := config.Default(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := config.DefaultSettings()
	s.CPUs = 8
	s.MemoryGiB = 16
	s.ApplyTo(cfg)

	withStartFlags(t, func() {
		startFlags.cpus = 2
		startFlags.memory = config.DefaultMemoryGiB // not changed
		applyFlagOverrides(cfg, changedSet("cpus"), false)
	})

	if cfg.CPUs != 2 {
		t.Errorf("CPUs = %d, want flag 2", cfg.CPUs)
	}
	if cfg.MemoryGiB != 16 {
		t.Errorf("MemoryGiB = %d, want persisted 16 (flag not changed)", cfg.MemoryGiB)
	}
}

// A boot/cartridge-driven start applies every flag verbatim regardless of the
// changed predicate, preserving the pre-resolved precedence (e.g. a --headless
// override of a GUI manifest stuffed into startFlags.gui).
func TestApplyFlagOverridesDrivenAppliesVerbatim(t *testing.T) {
	cfg, err := config.Default(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := config.DefaultSettings()
	s.CPUs = 8
	s.ApplyTo(cfg)
	cfg.GUI = true // pretend a manifest set GUI mode

	withStartFlags(t, func() {
		startFlags.cpus = 3
		startFlags.memory = 12
		startFlags.disk = 64
		startFlags.gui = false // boot resolved --headless
		startFlags.timeout = 7 * time.Minute
		// changedSet is empty: driven=true must apply anyway.
		applyFlagOverrides(cfg, changedSet(), true)
	})

	if cfg.CPUs != 3 {
		t.Errorf("CPUs = %d, want driven 3", cfg.CPUs)
	}
	if cfg.MemoryGiB != 12 {
		t.Errorf("MemoryGiB = %d, want driven 12", cfg.MemoryGiB)
	}
	if cfg.GUI {
		t.Error("GUI = true, want driven --headless (false)")
	}
	if cfg.WaitForIncus != 7*time.Minute {
		t.Errorf("WaitForIncus = %v, want driven 7m", cfg.WaitForIncus)
	}
}

func TestApplyFlagOverridesGuestAgent(t *testing.T) {
	tests := []struct {
		name     string
		changed  []string
		useAgent bool
		noAgent  bool
		settings bool // Settings.UseGuestAgent baseline
		want     bool
	}{
		{"neither changed keeps settings true", nil, false, false, true, true},
		{"neither changed keeps settings false", nil, false, false, false, false},
		{"use-guest-agent changed on", []string{"use-guest-agent"}, true, false, false, true},
		{"no-agent overrides use-guest-agent", []string{"use-guest-agent", "no-agent"}, true, true, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := config.Default(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			s := config.DefaultSettings()
			s.UseGuestAgent = tt.settings
			s.ApplyTo(cfg)

			withStartFlags(t, func() {
				startFlags.useAgent = tt.useAgent
				startFlags.noAgent = tt.noAgent
				applyFlagOverrides(cfg, changedSet(tt.changed...), false)
			})

			if cfg.UseGuestAgent != tt.want {
				t.Errorf("UseGuestAgent = %v, want %v", cfg.UseGuestAgent, tt.want)
			}
		})
	}
}

func TestApplyFlagOverridesImageURLClearsSHA(t *testing.T) {
	cfg, err := config.Default(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// config.Default for a supported arch carries the pinned Debian SHA-512.
	if cfg.BaseImageSHA512 == "" {
		t.Skip("no pinned SHA on this arch; nothing to clear")
	}

	withStartFlags(t, func() {
		startFlags.imageURL = "https://example.test/custom.qcow2"
		applyFlagOverrides(cfg, changedSet("image-url"), false)
	})

	if cfg.BaseImageURL != "https://example.test/custom.qcow2" {
		t.Errorf("BaseImageURL = %q", cfg.BaseImageURL)
	}
	if cfg.BaseImageSHA512 != "" {
		t.Errorf("BaseImageSHA512 = %q, want cleared for a custom URL", cfg.BaseImageSHA512)
	}
}

// An empty image-url flag must never clobber a Settings-provided image URL, even
// if somehow marked changed.
func TestApplyFlagOverridesEmptyImageURLNoClobber(t *testing.T) {
	cfg, err := config.Default(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := config.DefaultSettings()
	s.Image = config.ImageSource{Kind: config.ImageCustomURL, URL: "https://example.test/from-settings.qcow2"}
	s.ApplyTo(cfg)

	withStartFlags(t, func() {
		startFlags.imageURL = ""
		applyFlagOverrides(cfg, changedSet("image-url"), false)
	})

	if cfg.BaseImageURL != "https://example.test/from-settings.qcow2" {
		t.Errorf("BaseImageURL = %q, want settings URL preserved", cfg.BaseImageURL)
	}
}
