package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultSettingsIsValid(t *testing.T) {
	s := DefaultSettings()
	if err := s.Validate(); err != nil {
		t.Fatalf("DefaultSettings should be valid, got: %v", err)
	}
	if s.SchemaVersion != settingsSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", s.SchemaVersion, settingsSchemaVersion)
	}
}

// DefaultSettings must reproduce the built-in Config defaults so a fresh
// settings.json is a no-op overlay. This guards against the two default sources
// drifting apart.
func TestDefaultSettingsMatchesConfigDefault(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Default(dir)
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	before := *cfg
	DefaultSettings().ApplyTo(cfg)

	if cfg.CPUs != before.CPUs {
		t.Errorf("CPUs changed: %d -> %d", before.CPUs, cfg.CPUs)
	}
	if cfg.MemoryGiB != before.MemoryGiB {
		t.Errorf("MemoryGiB changed: %d -> %d", before.MemoryGiB, cfg.MemoryGiB)
	}
	if cfg.DiskSizeGiB != before.DiskSizeGiB {
		t.Errorf("DiskSizeGiB changed: %d -> %d", before.DiskSizeGiB, cfg.DiskSizeGiB)
	}
	if cfg.NetworkMode != before.NetworkMode {
		t.Errorf("NetworkMode changed: %q -> %q", before.NetworkMode, cfg.NetworkMode)
	}
	if cfg.NestedVirtDisabled != before.NestedVirtDisabled {
		t.Errorf("NestedVirtDisabled changed: %v -> %v", before.NestedVirtDisabled, cfg.NestedVirtDisabled)
	}
	if cfg.UseHostedGuestImage != before.UseHostedGuestImage {
		t.Errorf("UseHostedGuestImage changed: %v -> %v", before.UseHostedGuestImage, cfg.UseHostedGuestImage)
	}
	if cfg.WaitForIncus != before.WaitForIncus {
		t.Errorf("WaitForIncus changed: %v -> %v", before.WaitForIncus, cfg.WaitForIncus)
	}
}

func TestSettingsValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Settings)
		wantErr bool
	}{
		{"default", func(*Settings) {}, false},
		{"bad start policy", func(s *Settings) { s.StartPolicy = "whenever" }, true},
		{"empty start policy", func(s *Settings) { s.StartPolicy = "" }, true},
		{"bad network mode", func(s *Settings) { s.NetworkMode = "carrier-pigeon" }, true},
		{"bridged without iface", func(s *Settings) { s.NetworkMode = NetSettingBridged; s.BridgeInterface = "" }, true},
		{"bridged with iface", func(s *Settings) { s.NetworkMode = NetSettingBridged; s.BridgeInterface = "en0" }, false},
		{"bad nested virt", func(s *Settings) { s.NestedVirt = "maybe" }, true},
		{"cpus zero", func(s *Settings) { s.CPUs = 0 }, true},
		{"memory too low", func(s *Settings) { s.MemoryGiB = 1 }, true},
		{"disk too small", func(s *Settings) { s.DiskSizeGiB = MinDiskSizeGiB - 1 }, true},
		{"wait too short", func(s *Settings) { s.WaitForIncus = Duration(time.Millisecond) }, true},
		{"custom url image", func(s *Settings) { s.Image = ImageSource{Kind: ImageCustomURL, URL: "https://x/y.qcow2"} }, false},
		{"custom url image missing url", func(s *Settings) { s.Image = ImageSource{Kind: ImageCustomURL} }, true},
		{"local path image", func(s *Settings) { s.Image = ImageSource{Kind: ImageLocalPath, Path: "/tmp/x.raw"} }, false},
		{"local path image with url", func(s *Settings) {
			s.Image = ImageSource{Kind: ImageLocalPath, Path: "/tmp/x.raw", URL: "https://x"}
		}, true},
		{"hosted image with stray path", func(s *Settings) { s.Image = ImageSource{Kind: ImageHosted, Path: "/x"} }, true},
		{"unknown image kind", func(s *Settings) { s.Image = ImageSource{Kind: "magic"} }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := DefaultSettings()
			tt.mutate(&s)
			err := s.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

func TestLoadSettingsMissingReturnsDefaults(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadSettings(dir)
	if err != nil {
		t.Fatalf("LoadSettings on missing file should not error, got: %v", err)
	}
	if s != DefaultSettings() {
		t.Errorf("LoadSettings missing = %+v, want DefaultSettings %+v", s, DefaultSettings())
	}
}

func TestSettingsSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := DefaultSettings()
	want.StartPolicy = StartOnLaunch
	want.CPUs = 8
	want.MemoryGiB = 16
	want.NetworkMode = NetSettingBridged
	want.BridgeInterface = "en1"
	want.Image = ImageSource{Kind: ImageCustomURL, URL: "https://example.test/img.qcow2"}
	want.WaitForIncus = Duration(5 * time.Minute)

	if err := want.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// The atomic write must leave exactly settings.json and no temp residue.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != settingsFileName {
			t.Errorf("unexpected file left in state dir: %s", e.Name())
		}
	}

	got, err := LoadSettings(dir)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if got != want {
		t.Errorf("round trip mismatch:\n got = %+v\nwant = %+v", got, want)
	}
}

func TestLoadSettingsInvalidErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, settingsFileName)
	if err := os.WriteFile(path, []byte(`{"startPolicy":"nonsense","cpus":4,"memoryGiB":8,"diskSizeGiB":64,"networkMode":"shared","authMode":"oidc","nestedVirt":"auto","image":{"kind":"debian"},"waitForIncus":"10m"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSettings(dir); err == nil {
		t.Fatal("LoadSettings should reject a file with an invalid start policy")
	}
}

func TestLoadSettingsPartialKeepsDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, settingsFileName)
	// Only set CPUs; everything else should retain DefaultSettings values.
	if err := os.WriteFile(path, []byte(`{"cpus":12}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSettings(dir)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if got.CPUs != 12 {
		t.Errorf("CPUs = %d, want 12", got.CPUs)
	}
	if got.StartPolicy != StartManual {
		t.Errorf("StartPolicy = %q, want default %q", got.StartPolicy, StartManual)
	}
	if got.MemoryGiB != DefaultMemoryGiB {
		t.Errorf("MemoryGiB = %d, want default %d", got.MemoryGiB, DefaultMemoryGiB)
	}
}

func TestDurationJSON(t *testing.T) {
	var d Duration
	if err := d.UnmarshalJSON([]byte(`"90s"`)); err != nil {
		t.Fatalf("UnmarshalJSON string: %v", err)
	}
	if time.Duration(d) != 90*time.Second {
		t.Errorf("got %v, want 90s", time.Duration(d))
	}
	// Numeric nanoseconds tolerated.
	if err := d.UnmarshalJSON([]byte(`60000000000`)); err != nil {
		t.Fatalf("UnmarshalJSON number: %v", err)
	}
	if time.Duration(d) != time.Minute {
		t.Errorf("got %v, want 1m", time.Duration(d))
	}
	if err := d.UnmarshalJSON([]byte(`"not-a-duration"`)); err == nil {
		t.Error("UnmarshalJSON should reject a bad duration string")
	}
	b, err := Duration(10 * time.Minute).MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if string(b) != `"10m0s"` {
		t.Errorf("MarshalJSON = %s, want \"10m0s\"", b)
	}
}

func TestApplyToImageSources(t *testing.T) {
	tests := []struct {
		name  string
		image ImageSource
		check func(*testing.T, *Config)
	}{
		{"hosted", ImageSource{Kind: ImageHosted}, func(t *testing.T, c *Config) {
			if !c.UseHostedGuestImage {
				t.Error("UseHostedGuestImage should be true for hosted")
			}
			// The URL must be re-resolved to the hosted release asset (not left
			// pointing at the Debian default from Default()), and the pinned
			// Debian SHA-512 must be cleared so the fail-closed sidecar path runs.
			wantURL, err := HostedGuestImageURL(c.Arch)
			if err != nil {
				t.Fatalf("HostedGuestImageURL(%q): %v", c.Arch, err)
			}
			if c.BaseImageURL != wantURL {
				t.Errorf("BaseImageURL = %q, want hosted %q", c.BaseImageURL, wantURL)
			}
			if c.BaseImageSHA512 != "" {
				t.Errorf("hosted must clear the pinned SHA-512, got %q", c.BaseImageSHA512)
			}
			if c.BaseImagePath != "" {
				t.Errorf("hosted must clear BaseImagePath, got %q", c.BaseImagePath)
			}
		}},
		{"debian", ImageSource{Kind: ImageDebian}, func(t *testing.T, c *Config) {
			if c.UseHostedGuestImage {
				t.Error("UseHostedGuestImage should be false for debian")
			}
			// Default() now resolves the hosted URL, so selecting Debian must
			// re-point BaseImageURL back and restore the pinned SHA-512.
			wantURL, err := DebianTrixieGenericCloudURL(c.Arch)
			if err != nil {
				t.Skipf("no debian image for arch %s", c.Arch)
			}
			if c.BaseImageURL != wantURL {
				t.Errorf("BaseImageURL = %q, want debian %q", c.BaseImageURL, wantURL)
			}
			if c.BaseImageSHA512 != DebianTrixieGenericCloudSHA512(c.Arch) {
				t.Errorf("debian must restore the pinned SHA-512, got %q", c.BaseImageSHA512)
			}
		}},
		{"custom url", ImageSource{Kind: ImageCustomURL, URL: "https://x/y.qcow2"}, func(t *testing.T, c *Config) {
			if c.BaseImageURL != "https://x/y.qcow2" {
				t.Errorf("BaseImageURL = %q", c.BaseImageURL)
			}
			if c.BaseImageSHA512 != "" {
				t.Errorf("custom URL should clear SHA512, got %q", c.BaseImageSHA512)
			}
		}},
		{"local path", ImageSource{Kind: ImageLocalPath, Path: "/tmp/img.raw"}, func(t *testing.T, c *Config) {
			if c.BaseImagePath != "/tmp/img.raw" {
				t.Errorf("BaseImagePath = %q", c.BaseImagePath)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Default(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			s := DefaultSettings()
			s.Image = tt.image
			s.ApplyTo(cfg)
			tt.check(t, cfg)
		})
	}
}

func TestApplyToNilConfigNoPanic(t *testing.T) {
	DefaultSettings().ApplyTo(nil) // must not panic
}

func TestApplyToBridgedNetwork(t *testing.T) {
	cfg, err := Default(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := DefaultSettings()
	s.NetworkMode = NetSettingBridged
	s.BridgeInterface = "en5"
	s.ApplyTo(cfg)
	if cfg.NetworkMode != NetworkModeBridged {
		t.Errorf("NetworkMode = %q, want %q", cfg.NetworkMode, NetworkModeBridged)
	}
	if cfg.BridgeInterface != "en5" {
		t.Errorf("BridgeInterface = %q, want en5", cfg.BridgeInterface)
	}
}
