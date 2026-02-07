package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/ssh"
)

func TestDefaultBaseImageURL(t *testing.T) {
	tests := []struct {
		name    string
		arch    string
		wantURL string
		wantErr bool
	}{
		{
			name:    "arm64 returns Ubuntu 24.04 ARM64 image",
			arch:    "arm64",
			wantURL: "https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-arm64.img",
			wantErr: false,
		},
		{
			name:    "amd64 returns Ubuntu 24.04 AMD64 image",
			arch:    "amd64",
			wantURL: "https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-amd64.img",
			wantErr: false,
		},
		{
			name:    "unsupported architecture returns error",
			arch:    "riscv64",
			wantURL: "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DefaultBaseImageURL(tt.arch)
			if (err != nil) != tt.wantErr {
				t.Errorf("DefaultBaseImageURL() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.wantURL {
				t.Errorf("DefaultBaseImageURL() = %v, want %v", got, tt.wantURL)
			}
		})
	}
}

func TestDefaultConfig(t *testing.T) {
	tmpDir := t.TempDir()
	name := "test-vm"

	cfg, err := Default(tmpDir, name)
	if err != nil {
		t.Fatalf("Default() error = %v", err)
	}

	if cfg.Name != name {
		t.Errorf("Name = %v, want %v", cfg.Name, name)
	}
	if cfg.Hostname != name {
		t.Errorf("Hostname = %v, want %v", cfg.Hostname, name)
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
	if !cfg.GUI {
		t.Error("GUI should be enabled by default")
	}

	expectedVMDir := filepath.Join(tmpDir, name)
	if cfg.VMDir != expectedVMDir {
		t.Errorf("VMDir = %v, want %v", cfg.VMDir, expectedVMDir)
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
			setup:   func(c *Config) {},
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
			cfg, err := Default(tmpDir, "test-vm")
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

	cfg, err := Default("", "test-vm")
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

	cfg, err := Default("", "test-vm")
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

	cfg, err := Default("", "test-vm")
	if err != nil {
		t.Fatalf("Default() error = %v", err)
	}

	if cfg.StateDir != tmpDir {
		t.Errorf("StateDir = %v, want %v", cfg.StateDir, tmpDir)
	}
}
