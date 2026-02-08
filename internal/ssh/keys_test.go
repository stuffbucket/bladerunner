package ssh

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDir(t *testing.T) {
	t.Run("uses XDG_CONFIG_HOME if set", func(t *testing.T) {
		tmpDir := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", tmpDir)

		got := Dir()
		expected := filepath.Join(tmpDir, "bladerunner", "ssh")
		if got != expected {
			t.Errorf("Dir() = %q, want %q", got, expected)
		}
	})

	t.Run("falls back to home directory", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "")

		got := Dir()
		home, _ := os.UserHomeDir()
		expected := filepath.Join(home, ".config", "bladerunner", "ssh")
		if got != expected {
			t.Errorf("Dir() = %q, want %q", got, expected)
		}
	})
}

func TestConfigDir(t *testing.T) {
	t.Run("uses XDG_CONFIG_HOME if set", func(t *testing.T) {
		tmpDir := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", tmpDir)

		got := ConfigDir()
		expected := filepath.Join(tmpDir, "bladerunner")
		if got != expected {
			t.Errorf("ConfigDir() = %q, want %q", got, expected)
		}
	})
}

func TestEnsureKeyPair(t *testing.T) {
	// Use a temp directory as XDG_CONFIG_HOME to isolate tests
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	t.Run("creates new key pair", func(t *testing.T) {
		keyPair, err := EnsureKeyPair()
		if err != nil {
			t.Fatalf("EnsureKeyPair() error = %v", err)
		}

		// Check paths are set
		if keyPair.PrivateKeyPath == "" {
			t.Error("PrivateKeyPath is empty")
		}
		if keyPair.PublicKeyPath == "" {
			t.Error("PublicKeyPath is empty")
		}

		// Check public key format
		if !strings.HasPrefix(keyPair.PublicKey, "ssh-ed25519 ") {
			t.Errorf("PublicKey = %q, want prefix 'ssh-ed25519 '", keyPair.PublicKey)
		}
		if !strings.HasSuffix(keyPair.PublicKey, " bladerunner") {
			t.Errorf("PublicKey = %q, want suffix ' bladerunner'", keyPair.PublicKey)
		}

		// Check files exist
		if _, err := os.Stat(keyPair.PrivateKeyPath); err != nil {
			t.Errorf("private key file does not exist: %v", err)
		}
		if _, err := os.Stat(keyPair.PublicKeyPath); err != nil {
			t.Errorf("public key file does not exist: %v", err)
		}

		// Check private key permissions
		info, err := os.Stat(keyPair.PrivateKeyPath)
		if err != nil {
			t.Fatalf("stat private key: %v", err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("private key permissions = %o, want %o", mode, 0o600)
		}
	})

	t.Run("reuses existing key pair", func(t *testing.T) {
		// First call creates keys
		keyPair1, err := EnsureKeyPair()
		if err != nil {
			t.Fatalf("first EnsureKeyPair() error = %v", err)
		}

		// Second call should return same keys
		keyPair2, err := EnsureKeyPair()
		if err != nil {
			t.Fatalf("second EnsureKeyPair() error = %v", err)
		}

		if keyPair1.PublicKey != keyPair2.PublicKey {
			t.Error("second call returned different public key")
		}
		if keyPair1.PrivateKeyPath != keyPair2.PrivateKeyPath {
			t.Error("second call returned different private key path")
		}
	})
}

func TestWriteSSHConfig(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	configPath, err := WriteSSHConfig(6022, "testuser", "/path/to/key")
	if err != nil {
		t.Fatalf("WriteSSHConfig() error = %v", err)
	}

	// Check file exists
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config file: %v", err)
	}

	// Check content
	config := string(content)
	if !strings.Contains(config, "Host bladerunner") {
		t.Error("config missing 'Host bladerunner'")
	}
	if !strings.Contains(config, "Port 6022") {
		t.Error("config missing 'Port 6022'")
	}
	if !strings.Contains(config, "User testuser") {
		t.Error("config missing 'User testuser'")
	}
	if !strings.Contains(config, "IdentityFile /path/to/key") {
		t.Error("config missing 'IdentityFile /path/to/key'")
	}

	// Check permissions
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("stat config file: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("config permissions = %o, want %o", mode, 0o600)
	}
}

func TestCommand(t *testing.T) {
	cmd := Command("/path/to/config")
	expected := "ssh -F /path/to/config bladerunner"
	if cmd != expected {
		t.Errorf("Command() = %q, want %q", cmd, expected)
	}
}
