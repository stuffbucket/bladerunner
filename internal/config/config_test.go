package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/ssh"
)

// pinnedURL builds the expected pinned genericcloud URL for an arch so the
// tests track DebianTrixieBuild automatically when the pin is bumped.
func pinnedURL(arch string) string {
	return fmt.Sprintf(
		"https://cloud.debian.org/images/cloud/trixie/%s/debian-13-genericcloud-%s-%s.qcow2",
		DebianTrixieBuild, arch, DebianTrixieBuild)
}

func TestDefaultBaseImageURL(t *testing.T) {
	// As of #155 the built-in default is the pre-baked hosted guest image; the
	// forced-cloud-init escape hatch selects the pinned Debian genericcloud.
	t.Run("default is the pre-baked hosted image", func(t *testing.T) {
		t.Setenv(ForceCloudInitEnvVar, "")
		for _, arch := range []string{"arm64", "amd64"} {
			got, err := DefaultBaseImageURL(arch)
			if err != nil {
				t.Fatalf("DefaultBaseImageURL(%s) error = %v", arch, err)
			}
			want, _ := HostedGuestImageURL(arch)
			if got != want {
				t.Errorf("DefaultBaseImageURL(%s) = %q, want hosted %q", arch, got, want)
			}
		}
	})

	t.Run("forced cloud-init selects pinned Debian", func(t *testing.T) {
		t.Setenv(ForceCloudInitEnvVar, "1")
		for _, arch := range []string{"arm64", "amd64"} {
			got, err := DefaultBaseImageURL(arch)
			if err != nil {
				t.Fatalf("DefaultBaseImageURL(%s) error = %v", arch, err)
			}
			if got != pinnedURL(arch) {
				t.Errorf("DefaultBaseImageURL(%s) = %q, want pinned Debian %q", arch, got, pinnedURL(arch))
			}
		}
	})

	t.Run("unsupported architecture returns error", func(t *testing.T) {
		t.Setenv(ForceCloudInitEnvVar, "")
		if _, err := DefaultBaseImageURL("riscv64"); err == nil {
			t.Error("DefaultBaseImageURL(riscv64) expected error, got nil")
		}
	})
}

func TestDebianTrixieGenericCloudURL(t *testing.T) {
	tests := []struct {
		arch    string
		wantURL string
		wantErr bool
	}{
		{"arm64", pinnedURL("arm64"), false},
		{"amd64", pinnedURL("amd64"), false},
		{"riscv64", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.arch, func(t *testing.T) {
			got, err := DebianTrixieGenericCloudURL(tt.arch)
			if (err != nil) != tt.wantErr {
				t.Errorf("DebianTrixieGenericCloudURL() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.wantURL {
				t.Errorf("DebianTrixieGenericCloudURL() = %q, want %q", got, tt.wantURL)
			}
		})
	}
}

// TestDebianTrixieImageIsPinned guards the reproducibility fix: the default
// image URL must point at a specific dated snapshot, never the rolling
// "latest" pointer, and must carry an embedded SHA-512 for verification.
func TestDebianTrixieImageIsPinned(t *testing.T) {
	for _, arch := range []string{"arm64", "amd64"} {
		url, err := DebianTrixieGenericCloudURL(arch)
		if err != nil {
			t.Fatalf("DebianTrixieGenericCloudURL(%s) error = %v", arch, err)
		}
		if strings.Contains(url, "/latest/") {
			t.Errorf("%s image URL uses rolling /latest/ (not reproducible): %s", arch, url)
		}
		if !strings.Contains(url, DebianTrixieBuild) {
			t.Errorf("%s image URL not pinned to DebianTrixieBuild %q: %s", arch, DebianTrixieBuild, url)
		}
		sum := DebianTrixieGenericCloudSHA512(arch)
		if len(sum) != 128 { // SHA-512 = 64 bytes = 128 hex chars
			t.Errorf("%s pinned SHA-512 should be 128 hex chars, got %d (%q)", arch, len(sum), sum)
		}
	}
	if DebianTrixieGenericCloudSHA512("riscv64") != "" {
		t.Errorf("unknown arch should have empty SHA-512")
	}
}

// TestDefaultConfigCarriesPinnedChecksum verifies the forced-cloud-init path
// wires the embedded SHA-512 for the host arch so the Debian download is
// verified. (The default hosted path carries no embedded SHA-512; it is verified
// fail-closed against its sidecar in EnsureBaseImage — see the vm package tests.)
func TestDefaultConfigCarriesPinnedChecksum(t *testing.T) {
	want := DebianTrixieGenericCloudSHA512(runtime.GOARCH)
	if want == "" {
		t.Skipf("no pinned checksum for arch %s", runtime.GOARCH)
	}
	t.Setenv(ForceCloudInitEnvVar, "1")
	cfg, err := Default(t.TempDir())
	if err != nil {
		t.Fatalf("Default() error = %v", err)
	}
	if cfg.BaseImageSHA512 != want {
		t.Errorf("Default().BaseImageSHA512 = %q, want %q", cfg.BaseImageSHA512, want)
	}
	if cfg.UseHostedGuestImage {
		t.Error("forced cloud-init: UseHostedGuestImage should be false")
	}
	if cfg.UseGuestAgent {
		t.Error("forced cloud-init: UseGuestAgent should be false")
	}
	if cfg.HostedImageFallback {
		t.Error("forced cloud-init: HostedImageFallback should be false")
	}
}

// TestDefaultConfigDefaultsToHostedImage verifies the #155 default: a fresh
// install uses the pre-baked hosted image + agent handshake with the automatic
// Debian fallback armed, and carries no embedded SHA-512 (sidecar-verified).
func TestDefaultConfigDefaultsToHostedImage(t *testing.T) {
	t.Setenv(ForceCloudInitEnvVar, "")
	cfg, err := Default(t.TempDir())
	if err != nil {
		t.Fatalf("Default() error = %v", err)
	}
	if !cfg.UseHostedGuestImage {
		t.Error("default should use the pre-baked hosted guest image")
	}
	if !cfg.UseGuestAgent {
		t.Error("default should use the guest agent handshake path")
	}
	if !cfg.HostedImageFallback {
		t.Error("default should arm the hosted->Debian auto-fallback")
	}
	if cfg.BaseImageSHA512 != "" {
		t.Errorf("hosted default should carry no embedded SHA-512, got %q", cfg.BaseImageSHA512)
	}
	wantURL, _ := HostedGuestImageURL(runtime.GOARCH)
	if cfg.BaseImageURL != wantURL {
		t.Errorf("default BaseImageURL = %q, want hosted %q", cfg.BaseImageURL, wantURL)
	}
}

func TestHostedGuestImageURL(t *testing.T) {
	tests := []struct {
		arch    string
		wantURL string
		wantErr bool
	}{
		{"arm64", "https://github.com/stuffbucket/bladerunner/releases/download/guest-image-latest/bladerunner-guest-arm64.qcow2", false},
		{"amd64", "https://github.com/stuffbucket/bladerunner/releases/download/guest-image-latest/bladerunner-guest-amd64.qcow2", false},
		{"riscv64", "", true},
		{"", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.arch, func(t *testing.T) {
			got, err := HostedGuestImageURL(tt.arch)
			if (err != nil) != tt.wantErr {
				t.Errorf("HostedGuestImageURL() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.wantURL {
				t.Errorf("HostedGuestImageURL() = %q, want %q", got, tt.wantURL)
			}
		})
	}
}

func TestResolveBaseImageURL(t *testing.T) {
	// useHosted=false should match the Debian genericcloud URL.
	got, err := ResolveBaseImageURL("arm64", false)
	if err != nil {
		t.Fatalf("ResolveBaseImageURL(arm64, false) error = %v", err)
	}
	want, _ := DebianTrixieGenericCloudURL("arm64")
	if got != want {
		t.Errorf("ResolveBaseImageURL(arm64, false) = %q, want %q", got, want)
	}

	// useHosted=true should match the GitHub Release URL.
	got, err = ResolveBaseImageURL("amd64", true)
	if err != nil {
		t.Fatalf("ResolveBaseImageURL(amd64, true) error = %v", err)
	}
	want, _ = HostedGuestImageURL("amd64")
	if got != want {
		t.Errorf("ResolveBaseImageURL(amd64, true) = %q, want %q", got, want)
	}

	if _, err := ResolveBaseImageURL("riscv64", false); err == nil {
		t.Error("ResolveBaseImageURL(riscv64, false) expected error, got nil")
	}
	if _, err := ResolveBaseImageURL("riscv64", true); err == nil {
		t.Error("ResolveBaseImageURL(riscv64, true) expected error, got nil")
	}
}

func TestDefaultConfigUseHostedGuestImage(t *testing.T) {
	t.Setenv(ForceCloudInitEnvVar, "")
	cfg, err := Default(t.TempDir())
	if err != nil {
		t.Fatalf("Default() error = %v", err)
	}
	if !cfg.UseHostedGuestImage {
		t.Error("Default config should use the pre-baked hosted guest image (#155)")
	}
}

func TestDefaultAptMirrorURI(t *testing.T) {
	const want = "http://deb.debian.org/debian"
	for _, arch := range []string{"arm64", "amd64", "riscv64", ""} {
		t.Run("arch="+arch, func(t *testing.T) {
			if got := DefaultAptMirrorURI(arch); got != want {
				t.Errorf("DefaultAptMirrorURI(%q) = %q, want %q", arch, got, want)
			}
		})
	}
}

func TestDefaultConfig(t *testing.T) {
	t.Setenv(ForceCloudInitEnvVar, "")
	tmpDir := t.TempDir()

	cfg, err := Default(tmpDir)
	if err != nil {
		t.Fatalf("Default() error = %v", err)
	}

	if cfg.Name != "bladerunner" {
		t.Errorf("Name = %v, want bladerunner", cfg.Name)
	}
	if cfg.Hostname != "bladerunner" {
		t.Errorf("Hostname = %v, want bladerunner", cfg.Hostname)
	}
	if cfg.CPUs != 4 {
		t.Errorf("CPUs = %v, want 4", cfg.CPUs)
	}
	if cfg.MemoryGiB != 8 {
		t.Errorf("MemoryGiB = %v, want 8", cfg.MemoryGiB)
	}
	if cfg.DiskSizeGiB != 64 {
		t.Errorf("DiskSizeGiB = %v, want 64", cfg.DiskSizeGiB)
	}
	if cfg.GUI {
		t.Error("GUI (serial console window) should be OFF by default; opt in via Settings.ShowConsole or --gui")
	}

	if cfg.VMDir != tmpDir {
		t.Errorf("VMDir = %v, want %v", cfg.VMDir, tmpDir)
	}

	expectedURL, _ := DefaultBaseImageURL(runtime.GOARCH)
	if cfg.BaseImageURL != expectedURL {
		t.Errorf("BaseImageURL = %v, want %v", cfg.BaseImageURL, expectedURL)
	}
}

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(*Config)
		wantErr bool
	}{
		{
			name:    "valid config passes",
			setup:   func(_ *Config) {},
			wantErr: false,
		},
		{
			name: "missing name fails",
			setup: func(c *Config) {
				c.Name = ""
			},
			wantErr: true,
		},
		{
			name: "zero CPUs fails",
			setup: func(c *Config) {
				c.CPUs = 0
			},
			wantErr: true,
		},
		{
			name: "invalid network mode fails",
			setup: func(c *Config) {
				c.NetworkMode = "invalid"
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			cfg, err := Default(tmpDir)
			if err != nil {
				t.Fatalf("Default() error = %v", err)
			}

			// Set up SSH keys for validation
			keyPair, err := ssh.EnsureKeyPair()
			if err != nil {
				t.Fatalf("EnsureKeyPair() error = %v", err)
			}
			cfg.SetSSHKeys(keyPair.PublicKey, keyPair.PrivateKeyPath)

			tt.setup(cfg)
			err = cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSSHKeyDetection(t *testing.T) {
	keyPair, err := ssh.EnsureKeyPair()
	if err != nil {
		t.Fatalf("EnsureKeyPair() failed: %v", err)
	}

	if keyPair.PublicKey == "" {
		t.Error("EnsureKeyPair() returned empty public key")
	}
	if len(keyPair.PublicKey) < 50 {
		t.Errorf("SSH key seems too short: %d bytes", len(keyPair.PublicKey))
	}
	if keyPair.PrivateKeyPath == "" {
		t.Error("EnsureKeyPair() returned empty private key path")
	}
}

func TestStateDirectoryDefault(t *testing.T) {
	// Clear env vars that would override the default
	t.Setenv("BLADERUNNER_STATE_DIR", "")
	t.Setenv("XDG_STATE_HOME", "")

	cfg, err := Default("")
	if err != nil {
		t.Fatalf("Default() error = %v", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("Cannot get home directory")
	}

	expectedBase := filepath.Join(home, ".local", "state", "bladerunner")
	if cfg.StateDir != expectedBase {
		t.Errorf("StateDir = %v, want %v", cfg.StateDir, expectedBase)
	}
}

func TestStateDirectoryXDG(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("BLADERUNNER_STATE_DIR", "")
	t.Setenv("XDG_STATE_HOME", tmpDir)

	cfg, err := Default("")
	if err != nil {
		t.Fatalf("Default() error = %v", err)
	}

	expectedBase := filepath.Join(tmpDir, "bladerunner")
	if cfg.StateDir != expectedBase {
		t.Errorf("StateDir = %v, want %v", cfg.StateDir, expectedBase)
	}
}

func TestStateDirectoryEnvOverride(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("BLADERUNNER_STATE_DIR", tmpDir)

	cfg, err := Default("")
	if err != nil {
		t.Fatalf("Default() error = %v", err)
	}

	if cfg.StateDir != tmpDir {
		t.Errorf("StateDir = %v, want %v", cfg.StateDir, tmpDir)
	}
}
