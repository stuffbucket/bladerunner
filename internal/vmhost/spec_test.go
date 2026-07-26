package vmhost

import (
	"strings"
	"testing"
	"time"

	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/instance"
)

// flatSpec returns a minimal valid Spec for the flat default instance, which
// every rejection test then breaks in exactly one way.
func flatSpec() Spec {
	return Spec{Kind: instance.KindFlat}
}

// drivenSpec returns a valid boot-driven Spec (sizing pre-resolved), the shape
// `br boot` produces.
func drivenSpec() Spec {
	s := Spec{Kind: instance.KindDisk, Driven: true}
	s.Overrides.CPUs = config.DefaultCPUs
	s.Overrides.MemoryGiB = config.DefaultMemoryGiB
	s.Overrides.DiskSizeGiB = config.DefaultDiskSizeGiB
	return s
}

// cartridgeSpec returns a valid cartridge Spec.
func cartridgeSpec() Spec {
	s := drivenSpec()
	s.Kind = instance.KindCartridge
	s.CartridgePath = "/tmp/demo.dmg"
	s.Mountpoint = "/state/mnt/demo"
	return s
}

func TestSpecValidate(t *testing.T) {
	t.Setenv(config.ForceHostedImageEnvVar, "")
	t.Setenv(config.ForceDebianImageEnvVar, "")

	tests := []struct {
		name    string
		spec    func() Spec
		wantErr string // substring; "" means the spec must be accepted
	}{
		{name: "flat default", spec: flatSpec},
		{name: "driven disk", spec: drivenSpec},
		{name: "cartridge", spec: cartridgeSpec},
		{
			name:    "unknown kind",
			spec:    func() Spec { return Spec{Kind: "floppy"} },
			wantErr: `unknown instance kind "floppy"`,
		},
		{
			name:    "empty kind",
			spec:    func() Spec { return Spec{} },
			wantErr: "unknown instance kind",
		},
		{
			name:    "invalid name",
			spec:    func() Spec { s := flatSpec(); s.Name = "../escape"; return s },
			wantErr: "invalid instance name",
		},
		{
			name:    "name with separator",
			spec:    func() Spec { s := flatSpec(); s.Name = "a/b"; return s },
			wantErr: "path separator",
		},
		{
			name:    "cartridge path without cartridge kind",
			spec:    func() Spec { s := flatSpec(); s.CartridgePath = "/tmp/x.dmg"; return s },
			wantErr: "cartridge path",
		},
		{
			name:    "mountpoint without cartridge kind",
			spec:    func() Spec { s := flatSpec(); s.Mountpoint = "/state/mnt/x"; return s },
			wantErr: "mountpoint",
		},
		{
			name:    "cartridge without path",
			spec:    func() Spec { s := cartridgeSpec(); s.CartridgePath = ""; return s },
			wantErr: "needs a cartridge path",
		},
		{
			name:    "cartridge without mountpoint",
			spec:    func() Spec { s := cartridgeSpec(); s.Mountpoint = ""; return s },
			wantErr: "needs a mountpoint",
		},
		{
			name:    "cartridge cannot restore",
			spec:    func() Spec { s := cartridgeSpec(); s.RestoreFrom = "/state/saved.bin"; return s },
			wantErr: "cold-boots",
		},
		{
			name:    "driven without cpus",
			spec:    func() Spec { s := drivenSpec(); s.Overrides.CPUs = 0; return s },
			wantErr: "must resolve CPUs",
		},
		{
			name:    "driven without memory",
			spec:    func() Spec { s := drivenSpec(); s.Overrides.MemoryGiB = 0; return s },
			wantErr: "must resolve memory",
		},
		{
			name:    "driven without disk size",
			spec:    func() Spec { s := drivenSpec(); s.Overrides.DiskSizeGiB = 0; return s },
			wantErr: "must resolve the disk size",
		},
		{
			name:    "negative port",
			spec:    func() Spec { s := flatSpec(); s.Ports.SSH = -1; return s },
			wantErr: "ssh port -1 is out of range",
		},
		{
			name:    "port above the maximum",
			spec:    func() Spec { s := flatSpec(); s.Ports.API = maxPort + 1; return s },
			wantErr: "api port 65536 is out of range",
		},
		{
			name:    "negative drain timeout",
			spec:    func() Spec { s := flatSpec(); s.DrainTimeout = -time.Second; return s },
			wantErr: "drain timeout",
		},
		{
			name: "config carrying live listeners",
			spec: func() Spec {
				s := flatSpec()
				s.Config = &config.Config{HostListeners: config.HostListeners{}}
				return s
			},
			wantErr: "live host listeners",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec().Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want an error mentioning %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// Every structural rejection wraps ErrInvalidSpec so a caller can tell a bad
// request apart from a failed start.
func TestSpecValidateWrapsSentinel(t *testing.T) {
	t.Setenv(config.ForceHostedImageEnvVar, "")
	t.Setenv(config.ForceDebianImageEnvVar, "")

	s := flatSpec()
	s.Name = "Not Valid"
	err := s.Validate()
	if err == nil {
		t.Fatal("expected a rejection")
	}
	if !strings.Contains(err.Error(), ErrInvalidSpec.Error()) {
		t.Fatalf("Validate() = %v, want it to wrap %v", err, ErrInvalidSpec)
	}
}

// New must refuse an invalid spec rather than hand back a Host that will fail
// halfway through a boot.
func TestNewRejectsInvalidSpec(t *testing.T) {
	t.Setenv(config.ForceHostedImageEnvVar, "")
	t.Setenv(config.ForceDebianImageEnvVar, "")

	if _, err := New(Spec{}); err == nil {
		t.Fatal("New(Spec{}) = nil error, want a rejection")
	}
	h, err := New(flatSpec())
	if err != nil {
		t.Fatalf("New(flatSpec()) = %v", err)
	}
	if h == nil {
		t.Fatal("New returned a nil host with no error")
	}
}

// changedSpec returns a Spec whose Overrides start from the given base and
// whose ChangedFlags are the named ones, standing in for cobra's
// cmd.Flags().Changed.
func changedSpec(o Overrides, driven bool, changed ...string) Spec {
	return Spec{Kind: instance.KindFlat, Overrides: o, ChangedFlags: changed, Driven: driven}
}

// On a plain `br start` with no flags changed, applyOverrides must leave the
// persisted Settings baseline intact (nothing is clobbered by flag defaults).
func TestApplyOverridesPlainNoChangeKeepsSettings(t *testing.T) {
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

	// The overrides hold cobra defaults the user didn't touch.
	changedSpec(Overrides{
		CPUs:        config.DefaultCPUs,
		MemoryGiB:   config.DefaultMemoryGiB,
		DiskSizeGiB: config.DefaultDiskSizeGiB,
		Timeout:     config.DefaultTimeout,
	}, false).applyOverrides(cfg)

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
func TestApplyOverridesPlainChangedWins(t *testing.T) {
	cfg, err := config.Default(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := config.DefaultSettings()
	s.CPUs = 8
	s.MemoryGiB = 16
	s.ApplyTo(cfg)

	changedSpec(Overrides{
		CPUs:      2,
		MemoryGiB: config.DefaultMemoryGiB, // not changed
	}, false, "cpus").applyOverrides(cfg)

	if cfg.CPUs != 2 {
		t.Errorf("CPUs = %d, want flag 2", cfg.CPUs)
	}
	if cfg.MemoryGiB != 16 {
		t.Errorf("MemoryGiB = %d, want persisted 16 (flag not changed)", cfg.MemoryGiB)
	}
}

// A boot/cartridge-driven start applies every override verbatim regardless of
// ChangedFlags, preserving the pre-resolved precedence (e.g. a --headless
// override of a GUI manifest stuffed into Overrides.GUI).
func TestApplyOverridesDrivenAppliesVerbatim(t *testing.T) {
	cfg, err := config.Default(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := config.DefaultSettings()
	s.CPUs = 8
	s.ApplyTo(cfg)
	cfg.GUI = true // pretend a manifest set GUI mode

	// ChangedFlags is empty: driven=true must apply anyway.
	changedSpec(Overrides{
		CPUs:        3,
		MemoryGiB:   12,
		DiskSizeGiB: 64,
		GUI:         false, // boot resolved --headless
		Timeout:     7 * time.Minute,
	}, true).applyOverrides(cfg)

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

func TestApplyOverridesImageURLClearsSHA(t *testing.T) {
	cfg, err := config.Default(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Default() is now the hosted image (no embedded SHA). Start from the Debian
	// baseline so the config carries the pinned SHA-512 we expect --image-url to
	// clear when it overrides with a custom URL.
	s := config.DefaultSettings()
	s.Image = config.ImageSource{Kind: config.ImageDebian}
	s.ApplyTo(cfg)
	if cfg.BaseImageSHA512 == "" {
		t.Skip("no pinned SHA on this arch; nothing to clear")
	}

	changedSpec(Overrides{ImageURL: "https://example.test/custom.qcow2"}, false, "image-url").applyOverrides(cfg)

	if cfg.BaseImageURL != "https://example.test/custom.qcow2" {
		t.Errorf("BaseImageURL = %q", cfg.BaseImageURL)
	}
	if cfg.BaseImageSHA512 != "" {
		t.Errorf("BaseImageSHA512 = %q, want cleared for a custom URL", cfg.BaseImageSHA512)
	}
}

// An empty image-url override must never clobber a Settings-provided image URL,
// even if somehow marked changed.
func TestApplyOverridesEmptyImageURLNoClobber(t *testing.T) {
	cfg, err := config.Default(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := config.DefaultSettings()
	s.Image = config.ImageSource{Kind: config.ImageCustomURL, URL: "https://example.test/from-settings.qcow2"}
	s.ApplyTo(cfg)

	changedSpec(Overrides{ImageURL: ""}, false, "image-url").applyOverrides(cfg)

	if cfg.BaseImageURL != "https://example.test/from-settings.qcow2" {
		t.Errorf("BaseImageURL = %q, want settings URL preserved", cfg.BaseImageURL)
	}
}

// --hosted-image forces the pre-baked hosted image even when the persisted
// Settings selected the Debian default, re-resolving the hosted URL for the arch
// and clearing the pinned SHA-512 so the fail-closed sidecar path applies.
func TestApplyOverridesHostedImageForce(t *testing.T) {
	t.Setenv(config.ForceHostedImageEnvVar, "")
	cfg, err := config.Default(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Start from the Debian baseline so we prove --hosted-image flips it.
	s := config.DefaultSettings()
	s.Image = config.ImageSource{Kind: config.ImageDebian}
	s.ApplyTo(cfg)
	if cfg.UseHostedGuestImage {
		t.Fatal("precondition: expected the Debian baseline before --hosted-image")
	}

	changedSpec(Overrides{HostedImage: true}, false, "hosted-image").applyOverrides(cfg)

	if !cfg.UseHostedGuestImage {
		t.Error("--hosted-image must set UseHostedGuestImage")
	}
	if cfg.BaseImageSHA512 != "" {
		t.Errorf("--hosted-image must clear the pinned SHA-512, got %q", cfg.BaseImageSHA512)
	}
	if cfg.BaseImagePath != "" {
		t.Errorf("--hosted-image must clear BaseImagePath, got %q", cfg.BaseImagePath)
	}
	wantURL, _ := config.HostedGuestImageURL(cfg.Arch)
	if cfg.BaseImageURL != wantURL {
		t.Errorf("BaseImageURL = %q, want hosted %q", cfg.BaseImageURL, wantURL)
	}
}

// BLADERUNNER_FORCE_HOSTED_IMAGE=1 forces the hosted image without the flag,
// exactly like --hosted-image (the non-interactive equivalent for e2e).
func TestApplyOverridesHostedImageForceViaEnv(t *testing.T) {
	t.Setenv(config.ForceHostedImageEnvVar, "1")
	t.Setenv(config.ForceDebianImageEnvVar, "")
	cfg, err := config.Default(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Default() is now hosted; start from the Debian baseline so the env force is
	// proven to flip it back to hosted.
	s := config.DefaultSettings()
	s.Image = config.ImageSource{Kind: config.ImageDebian}
	s.ApplyTo(cfg)
	if cfg.UseHostedGuestImage {
		t.Fatal("precondition: expected the Debian baseline before the hosted force env")
	}

	// env, not flag
	changedSpec(Overrides{HostedImage: false}, false).applyOverrides(cfg)

	if !cfg.UseHostedGuestImage {
		t.Error("BLADERUNNER_FORCE_HOSTED_IMAGE=1 must set UseHostedGuestImage")
	}
	wantURL, _ := config.HostedGuestImageURL(cfg.Arch)
	if cfg.BaseImageURL != wantURL {
		t.Errorf("BaseImageURL = %q, want hosted %q", cfg.BaseImageURL, wantURL)
	}
}

// TestApplyOverridesDebianImageForce verifies the --debian-image escape
// hatch (and its force env) flips the hosted default back to the verified Debian
// + cloud-init path, restoring the pinned SHA-512 and re-resolving the URL.
func TestApplyOverridesDebianImageForce(t *testing.T) {
	t.Setenv(config.ForceHostedImageEnvVar, "")
	t.Setenv(config.ForceDebianImageEnvVar, "")
	cfg, err := config.Default(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.UseHostedGuestImage {
		t.Fatal("precondition: Default() is expected to be hosted now")
	}

	changedSpec(Overrides{DebianImage: true}, false, "debian-image").applyOverrides(cfg)

	if cfg.UseHostedGuestImage {
		t.Error("--debian-image must disarm UseHostedGuestImage")
	}
	wantURL, err := config.DebianTrixieGenericCloudURL(cfg.Arch)
	if err != nil {
		t.Skipf("no debian image for arch %s", cfg.Arch)
	}
	if cfg.BaseImageURL != wantURL {
		t.Errorf("BaseImageURL = %q, want debian %q", cfg.BaseImageURL, wantURL)
	}
	if cfg.BaseImageSHA512 != config.DebianTrixieGenericCloudSHA512(cfg.Arch) {
		t.Errorf("--debian-image must restore the pinned SHA-512, got %q", cfg.BaseImageSHA512)
	}
}

// TestApplyOverridesDebianImageForceViaEnv verifies BLADERUNNER_FORCE_DEBIAN_IMAGE=1
// forces the Debian escape hatch with no flag.
func TestApplyOverridesDebianImageForceViaEnv(t *testing.T) {
	t.Setenv(config.ForceHostedImageEnvVar, "")
	t.Setenv(config.ForceDebianImageEnvVar, "1")
	cfg, err := config.Default(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// env, not flag
	changedSpec(Overrides{DebianImage: false}, false).applyOverrides(cfg)

	if cfg.UseHostedGuestImage {
		t.Error("BLADERUNNER_FORCE_DEBIAN_IMAGE=1 must disarm UseHostedGuestImage")
	}
}

// Validate rejects --hosted-image / --debian-image combined with each other or
// with a conflicting --image-url/--image-path, whether the force is requested
// via the flag or the env.
func TestValidateImageOverrideConflicts(t *testing.T) {
	tests := []struct {
		name        string
		hostedFlag  bool
		debianFlag  bool
		hostedEnv   string
		debianEnv   string
		imageURL    string
		imagePath   string
		wantErr     bool
		wantErrText string
	}{
		{name: "no override", wantErr: false},
		{name: "hosted flag alone", hostedFlag: true, wantErr: false},
		{name: "hosted env alone", hostedEnv: "1", wantErr: false},
		{name: "debian flag alone", debianFlag: true, wantErr: false},
		{name: "debian env alone", debianEnv: "1", wantErr: false},
		{name: "image-url alone", imageURL: "https://x.test/i.qcow2", wantErr: false},
		{name: "image-path alone", imagePath: "/tmp/i.qcow2", wantErr: false},
		{name: "hosted flag + debian flag", hostedFlag: true, debianFlag: true, wantErr: true, wantErrText: "--debian-image"},
		{name: "hosted flag + debian env", hostedFlag: true, debianEnv: "1", wantErr: true, wantErrText: "--debian-image"},
		{name: "hosted env + debian flag", hostedEnv: "1", debianFlag: true, wantErr: true, wantErrText: "--debian-image"},
		{name: "hosted flag + image-url", hostedFlag: true, imageURL: "https://x.test/i.qcow2", wantErr: true, wantErrText: "--image-url"},
		{name: "hosted flag + image-path", hostedFlag: true, imagePath: "/tmp/i.qcow2", wantErr: true, wantErrText: "--image-path"},
		{name: "hosted env + image-url", hostedEnv: "1", imageURL: "https://x.test/i.qcow2", wantErr: true, wantErrText: "--image-url"},
		{name: "debian flag + image-url", debianFlag: true, imageURL: "https://x.test/i.qcow2", wantErr: true, wantErrText: "--image-url"},
		{name: "debian flag + image-path", debianFlag: true, imagePath: "/tmp/i.qcow2", wantErr: true, wantErrText: "--image-path"},
		{name: "debian env + image-path", debianEnv: "1", imagePath: "/tmp/i.qcow2", wantErr: true, wantErrText: "--image-path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(config.ForceHostedImageEnvVar, tt.hostedEnv)
			t.Setenv(config.ForceDebianImageEnvVar, tt.debianEnv)
			s := flatSpec()
			s.Overrides = Overrides{
				HostedImage: tt.hostedFlag,
				DebianImage: tt.debianFlag,
				ImageURL:    tt.imageURL,
				ImagePath:   tt.imagePath,
			}
			err := s.validateImageOverrides()
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateImageOverrides() err = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), tt.wantErrText) {
				t.Errorf("expected error to mention %q, got %v", tt.wantErrText, err)
			}
		})
	}
}
