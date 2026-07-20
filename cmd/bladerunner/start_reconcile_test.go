package main

import (
	"strings"
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
			t.Setenv(config.ForceCloudInitEnvVar, "")
			t.Setenv(config.ForceHostedImageEnvVar, "")
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
	t.Setenv(config.ForceCloudInitEnvVar, "")
	t.Setenv(config.ForceHostedImageEnvVar, "")
	cfg, err := config.Default(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Force the pinned-Debian baseline (the default is now the hosted image,
	// which carries no embedded SHA-512) so there is a SHA-512 to clear.
	s := config.DefaultSettings()
	s.Image = config.ImageSource{Kind: config.ImageDebian}
	s.ApplyTo(cfg)
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
	if cfg.HostedImageFallback {
		t.Error("a custom --image-url must clear HostedImageFallback")
	}
	if cfg.UseHostedGuestImage {
		t.Error("a custom --image-url must clear UseHostedGuestImage")
	}
}

// The --cloud-init escape hatch forces the legacy Debian + first-boot cloud-init
// path off the #155 hosted default: it re-resolves the pinned Debian URL/SHA and
// disables the hosted image, the agent handshake, and the auto-fallback.
func TestApplyFlagOverridesCloudInitEscapeHatch(t *testing.T) {
	t.Setenv(config.ForceCloudInitEnvVar, "")
	t.Setenv(config.ForceHostedImageEnvVar, "")
	cfg, err := config.Default(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config.DefaultSettings().ApplyTo(cfg) // the hosted default baseline
	if !cfg.UseHostedGuestImage {
		t.Skip("forced cloud-init environment; hosted default not in effect")
	}

	withStartFlags(t, func() {
		startFlags.cloudInit = true
		applyFlagOverrides(cfg, changedSet("cloud-init"), false)
	})

	if cfg.UseHostedGuestImage {
		t.Error("--cloud-init must clear UseHostedGuestImage")
	}
	if cfg.UseGuestAgent {
		t.Error("--cloud-init must clear UseGuestAgent")
	}
	if cfg.HostedImageFallback {
		t.Error("--cloud-init must clear HostedImageFallback")
	}
	wantURL, _ := config.DebianTrixieGenericCloudURL(cfg.Arch)
	if cfg.BaseImageURL != wantURL {
		t.Errorf("BaseImageURL = %q, want pinned Debian %q", cfg.BaseImageURL, wantURL)
	}
	if cfg.BaseImageSHA512 != config.DebianTrixieGenericCloudSHA512(cfg.Arch) {
		t.Errorf("BaseImageSHA512 = %q, want pinned Debian checksum", cfg.BaseImageSHA512)
	}
}

// --hosted-image forces the pre-baked hosted image + agent path even when the
// persisted Settings selected the Debian (cloud-init) image, re-resolving the
// hosted URL and arming the auto-fallback.
func TestApplyFlagOverridesHostedImageForce(t *testing.T) {
	t.Setenv(config.ForceCloudInitEnvVar, "")
	t.Setenv(config.ForceHostedImageEnvVar, "")
	cfg, err := config.Default(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Start from the Debian/cloud-init baseline so we prove --hosted-image flips it.
	s := config.DefaultSettings()
	s.Image = config.ImageSource{Kind: config.ImageDebian}
	s.UseGuestAgent = false
	s.ApplyTo(cfg)
	if cfg.UseHostedGuestImage {
		t.Fatal("precondition: expected the Debian baseline before --hosted-image")
	}

	withStartFlags(t, func() {
		startFlags.hostedImage = true
		applyFlagOverrides(cfg, changedSet("hosted-image"), false)
	})

	if !cfg.UseHostedGuestImage {
		t.Error("--hosted-image must set UseHostedGuestImage")
	}
	if !cfg.UseGuestAgent {
		t.Error("--hosted-image must set UseGuestAgent (agent path)")
	}
	if !cfg.HostedImageFallback {
		t.Error("--hosted-image must arm the hosted->Debian auto-fallback")
	}
	if cfg.BaseImageSHA512 != "" {
		t.Errorf("--hosted-image must clear the pinned SHA-512, got %q", cfg.BaseImageSHA512)
	}
	wantURL, _ := config.HostedGuestImageURL(cfg.Arch)
	if cfg.BaseImageURL != wantURL {
		t.Errorf("BaseImageURL = %q, want hosted %q", cfg.BaseImageURL, wantURL)
	}
}

// --cloud-init and --hosted-image are mutually exclusive, whether requested via
// the flags or their force env vars.
func TestValidateImageOverrideFlagsMutualExclusion(t *testing.T) {
	tests := []struct {
		name      string
		cloudFlag bool
		hostFlag  bool
		cloudEnv  string
		hostEnv   string
		wantErr   bool
	}{
		{"neither", false, false, "", "", false},
		{"only cloud flag", true, false, "", "", false},
		{"only hosted flag", false, true, "", "", false},
		{"both flags", true, true, "", "", true},
		{"cloud flag + hosted env", true, false, "", "1", true},
		{"hosted flag + cloud env", false, true, "1", "", true},
		{"both envs", false, false, "1", "1", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(config.ForceCloudInitEnvVar, tt.cloudEnv)
			t.Setenv(config.ForceHostedImageEnvVar, tt.hostEnv)
			withStartFlags(t, func() {
				startFlags.cloudInit = tt.cloudFlag
				startFlags.hostedImage = tt.hostFlag
				err := validateImageOverrideFlags()
				if (err != nil) != tt.wantErr {
					t.Errorf("validateImageOverrideFlags() err = %v, wantErr %v", err, tt.wantErr)
				}
				if err != nil && !strings.Contains(err.Error(), "mutually exclusive") {
					t.Errorf("expected 'mutually exclusive' error, got %v", err)
				}
			})
		})
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
