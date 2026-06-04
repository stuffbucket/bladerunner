package disk

import (
	"runtime"
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/config"
)

func TestApplyTo(t *testing.T) {
	hostedURL, err := config.HostedGuestImageURL(runtime.GOARCH)
	if err != nil {
		t.Fatalf("hosted url: %v", err)
	}
	sha := strings.Repeat("a", 64)

	t.Run("hosted", func(t *testing.T) {
		cfg := mustDefault(t)
		m := &Manifest{Name: "h", Image: ImageSpec{Hosted: true}, Boot: BootSpec{Mode: BootModeHeadless}}
		if err := m.ApplyTo(cfg); err != nil {
			t.Fatal(err)
		}
		if !cfg.UseHostedGuestImage {
			t.Fatal("expected UseHostedGuestImage=true")
		}
		if cfg.BaseImageURL != hostedURL {
			t.Fatalf("BaseImageURL = %q, want %q", cfg.BaseImageURL, hostedURL)
		}
		if cfg.BaseImageExpectedSHA256 != "" {
			t.Fatalf("BaseImageExpectedSHA256 = %q, want empty", cfg.BaseImageExpectedSHA256)
		}
		if cfg.BaseImageSHA512 != "" {
			t.Fatalf("BaseImageSHA512 = %q, want empty", cfg.BaseImageSHA512)
		}
	})

	t.Run("arches with sha256", func(t *testing.T) {
		cfg := mustDefault(t)
		m := &Manifest{
			Name: "a",
			Image: ImageSpec{Arches: map[string]ArchImage{
				runtime.GOARCH: {URL: "https://x/img.qcow2", SHA256: sha},
			}},
			Boot: BootSpec{Mode: BootModeHeadless},
		}
		if err := m.ApplyTo(cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.UseHostedGuestImage {
			t.Fatal("expected UseHostedGuestImage=false")
		}
		if cfg.BaseImageURL != "https://x/img.qcow2" {
			t.Fatalf("BaseImageURL = %q", cfg.BaseImageURL)
		}
		if cfg.BaseImageExpectedSHA256 != sha {
			t.Fatalf("BaseImageExpectedSHA256 = %q, want %q", cfg.BaseImageExpectedSHA256, sha)
		}
	})

	t.Run("arches missing goarch", func(t *testing.T) {
		cfg := mustDefault(t)
		other := "arm64"
		if runtime.GOARCH == "arm64" {
			other = "amd64"
		}
		m := &Manifest{
			Name:  "x",
			Image: ImageSpec{Arches: map[string]ArchImage{other: {URL: "https://x"}}},
			Boot:  BootSpec{Mode: BootModeHeadless},
		}
		if err := m.ApplyTo(cfg); err == nil {
			t.Fatal("expected error for missing goarch")
		}
	})

	t.Run("sizing defaults preserved when zero", func(t *testing.T) {
		cfg := mustDefault(t)
		m := &Manifest{Name: "s", Image: ImageSpec{Hosted: true}, Boot: BootSpec{Mode: BootModeHeadless}}
		if err := m.ApplyTo(cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.CPUs != config.DefaultCPUs || cfg.MemoryGiB != config.DefaultMemoryGiB || cfg.DiskSizeGiB != config.DefaultDiskSizeGiB {
			t.Fatalf("defaults not preserved: cpus=%d mem=%d disk=%d", cfg.CPUs, cfg.MemoryGiB, cfg.DiskSizeGiB)
		}
	})

	t.Run("sizing override and validate", func(t *testing.T) {
		cfg := mustDefault(t)
		m := &Manifest{
			Name:  "v",
			Image: ImageSpec{Hosted: true},
			VM:    VMSpec{CPUs: 2, MemoryGiB: 4, DiskSizeGiB: 32},
			Boot:  BootSpec{Mode: BootModeHeadless},
		}
		if err := m.ApplyTo(cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.CPUs != 2 || cfg.MemoryGiB != 4 || cfg.DiskSizeGiB != 32 {
			t.Fatalf("override failed: cpus=%d mem=%d disk=%d", cfg.CPUs, cfg.MemoryGiB, cfg.DiskSizeGiB)
		}
		cfg.SetSSHKeys("ssh-ed25519 AAAA test", "/tmp/key")
		if err := cfg.Validate(); err != nil {
			t.Fatalf("config.Validate after apply: %v", err)
		}
	})

	t.Run("gui mode sets GUI true", func(t *testing.T) {
		cfg := mustDefault(t)
		m := &Manifest{Name: "g", Image: ImageSpec{Hosted: true}, Boot: BootSpec{Mode: BootModeGUI}}
		if err := m.ApplyTo(cfg); err != nil {
			t.Fatal(err)
		}
		if !cfg.GUI {
			t.Fatal("expected GUI=true")
		}
	})

	t.Run("headless mode sets GUI false", func(t *testing.T) {
		cfg := mustDefault(t)
		m := &Manifest{Name: "h2", Image: ImageSpec{Hosted: true}, Boot: BootSpec{Mode: BootModeHeadless}}
		if err := m.ApplyTo(cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.GUI {
			t.Fatal("expected GUI=false")
		}
	})
}

func TestApplyToPrecedence(t *testing.T) {
	cfg := mustDefault(t)
	origState, origVM, origDisk := cfg.StateDir, cfg.VMDir, cfg.DiskPath
	m := &Manifest{Name: "p", Image: ImageSpec{Hosted: true}, Boot: BootSpec{Mode: BootModeGUI}}
	if err := m.ApplyTo(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.StateDir != origState || cfg.VMDir != origVM || cfg.DiskPath != origDisk {
		t.Fatal("ApplyTo must not touch slot-isolation fields (StateDir/VMDir/DiskPath)")
	}
}

func mustDefault(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Default(t.TempDir())
	if err != nil {
		t.Fatalf("config.Default: %v", err)
	}
	return cfg
}
