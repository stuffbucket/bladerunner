// These tests live in the external test package because they need
// internal/ssh, and internal/ssh imports internal/config for the XDG config
// directory. An in-package test file importing ssh would be an import cycle.
package config_test

import (
	"testing"

	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/ssh"
)

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(*config.Config)
		wantErr bool
	}{
		{
			name:    "valid config passes",
			setup:   func(_ *config.Config) {},
			wantErr: false,
		},
		{
			name: "missing name fails",
			setup: func(c *config.Config) {
				c.Name = ""
			},
			wantErr: true,
		},
		{
			name: "zero CPUs fails",
			setup: func(c *config.Config) {
				c.CPUs = 0
			},
			wantErr: true,
		},
		{
			name: "invalid network mode fails",
			setup: func(c *config.Config) {
				c.NetworkMode = "invalid"
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// EnsureKeyPair generates into ssh.Dir(), which falls back to
			// $HOME/.config/bladerunner/ssh. Without this the test writes a
			// real private key into the developer's home directory.
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())

			tmpDir := t.TempDir()
			cfg, err := config.Default(tmpDir)
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
	// Sandbox the key material. Without this the test both writes into the
	// developer's real $HOME and, on a machine that already has a key, only
	// ever exercises the read-back branch of EnsureKeyPair.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

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
