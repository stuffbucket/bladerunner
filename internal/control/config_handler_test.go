package control

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/config"
)

func TestConfigGet(t *testing.T) {
	cfg := &config.Config{
		Name:              "test-vm",
		SSHUser:           "testuser",
		SSHPrivateKeyPath: "/home/test/.ssh/id_ed25519",
		SSHConfigPath:     "/home/test/.config/bladerunner/ssh/config",
		LocalSSHPort:      6022,
		LocalAPIPort:      18443,
		VMDir:             "/home/test/.local/state/bladerunner",
		StateDir:          "/home/test/.local/state/bladerunner",
		CPUs:              4,
		MemoryGiB:         8,
		DiskSizeGiB:       64,
		Arch:              "arm64",
		Hostname:          "bladerunner",
		NetworkMode:       "shared",
		LogPath:           "/home/test/.local/state/bladerunner/bladerunner.log",
		GUI:               false,
		BaseImageURL:      "https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-arm64.img",
		BaseImagePath:     "/home/test/.local/state/bladerunner/ubuntu-24.04-server-cloudimg-arm64.img",
		CloudInitISO:      "/home/test/.local/state/bladerunner/cloud-init.iso",
	}

	cr := NewConfigRouter(cfg)
	router := cr.Router()

	t.Run("known key returns value", func(t *testing.T) {
		req := &Request{Command: "get", Args: map[string]string{"0": "name"}}
		resp := router.Dispatch(context.Background(), req)
		if resp.Response != "test-vm" {
			t.Errorf("Response = %q, want %q", resp.Response, "test-vm")
		}
	})

	t.Run("ssh-config-path returns value", func(t *testing.T) {
		req := &Request{Command: "get", Args: map[string]string{"0": "ssh-config-path"}}
		resp := router.Dispatch(context.Background(), req)
		if resp.Response != cfg.SSHConfigPath {
			t.Errorf("Response = %q, want %q", resp.Response, cfg.SSHConfigPath)
		}
	})

	t.Run("integer field returns string", func(t *testing.T) {
		req := &Request{Command: "get", Args: map[string]string{"0": "local-ssh-port"}}
		resp := router.Dispatch(context.Background(), req)
		if resp.Response != "6022" {
			t.Errorf("Response = %q, want %q", resp.Response, "6022")
		}
	})

	t.Run("cpus returns value", func(t *testing.T) {
		req := &Request{Command: "get", Args: map[string]string{"0": "cpus"}}
		resp := router.Dispatch(context.Background(), req)
		if resp.Response != "4" {
			t.Errorf("Response = %q, want %q", resp.Response, "4")
		}
	})

	t.Run("memory-gib returns value", func(t *testing.T) {
		req := &Request{Command: "get", Args: map[string]string{"0": "memory-gib"}}
		resp := router.Dispatch(context.Background(), req)
		if resp.Response != "8" {
			t.Errorf("Response = %q, want %q", resp.Response, "8")
		}
	})

	t.Run("arch returns value", func(t *testing.T) {
		req := &Request{Command: "get", Args: map[string]string{"0": "arch"}}
		resp := router.Dispatch(context.Background(), req)
		if resp.Response != "arm64" {
			t.Errorf("Response = %q, want %q", resp.Response, "arm64")
		}
	})

	t.Run("gui returns bool string", func(t *testing.T) {
		req := &Request{Command: "get", Args: map[string]string{"0": "gui"}}
		resp := router.Dispatch(context.Background(), req)
		if resp.Response != "false" {
			t.Errorf("Response = %q, want %q", resp.Response, "false")
		}
	})

	t.Run("pid returns valid number", func(t *testing.T) {
		req := &Request{Command: "get", Args: map[string]string{"0": "pid"}}
		resp := router.Dispatch(context.Background(), req)
		if resp.Error != "" {
			t.Errorf("unexpected error: %s", resp.Error)
		}
		pid, err := strconv.Atoi(resp.Response)
		if err != nil || pid <= 0 {
			t.Errorf("Response = %q, want valid PID", resp.Response)
		}
	})

	t.Run("base-image-url returns value", func(t *testing.T) {
		req := &Request{Command: "get", Args: map[string]string{"0": "base-image-url"}}
		resp := router.Dispatch(context.Background(), req)
		if resp.Response != cfg.BaseImageURL {
			t.Errorf("Response = %q, want %q", resp.Response, cfg.BaseImageURL)
		}
	})

	t.Run("base-image-path returns value", func(t *testing.T) {
		req := &Request{Command: "get", Args: map[string]string{"0": "base-image-path"}}
		resp := router.Dispatch(context.Background(), req)
		if resp.Response != cfg.BaseImagePath {
			t.Errorf("Response = %q, want %q", resp.Response, cfg.BaseImagePath)
		}
	})

	t.Run("cloud-init-iso returns value", func(t *testing.T) {
		req := &Request{Command: "get", Args: map[string]string{"0": "cloud-init-iso"}}
		resp := router.Dispatch(context.Background(), req)
		if resp.Response != cfg.CloudInitISO {
			t.Errorf("Response = %q, want %q", resp.Response, cfg.CloudInitISO)
		}
	})

	t.Run("unknown key returns error", func(t *testing.T) {
		req := &Request{Command: "get", Args: map[string]string{"0": "nonexistent"}}
		resp := router.Dispatch(context.Background(), req)
		if resp.Error == "" {
			t.Error("expected error for unknown key")
		}
	})

	t.Run("missing key returns usage error", func(t *testing.T) {
		req := &Request{Command: "get", Args: map[string]string{}}
		resp := router.Dispatch(context.Background(), req)
		if resp.Error == "" {
			t.Error("expected error for missing key")
		}
	})

	t.Run("deferred key not yet set returns error", func(t *testing.T) {
		cfg2 := &config.Config{SSHConfigPath: ""}
		cr2 := NewConfigRouter(cfg2)
		req := &Request{Command: "get", Args: map[string]string{"0": "ssh-config-path"}}
		resp := cr2.Router().Dispatch(context.Background(), req)
		if resp.Error == "" {
			t.Error("expected error for deferred key not yet available")
		}
	})

	t.Run("sees value set after router creation", func(t *testing.T) {
		cfg3 := &config.Config{SSHConfigPath: ""}
		cr3 := NewConfigRouter(cfg3)

		// Simulate value being set after VM start
		cr3.Lock()
		cfg3.SSHConfigPath = "/later/path/config"
		cr3.Unlock()

		req := &Request{Command: "get", Args: map[string]string{"0": "ssh-config-path"}}
		resp := cr3.Router().Dispatch(context.Background(), req)
		if resp.Response != "/later/path/config" {
			t.Errorf("Response = %q, want %q", resp.Response, "/later/path/config")
		}
	})
}

func TestConfigSet(t *testing.T) {
	cfg := &config.Config{}
	cr := NewConfigRouter(cfg)

	req := &Request{Command: "set", Args: map[string]string{"0": "name", "1": "new-name"}}
	resp := cr.Router().Dispatch(context.Background(), req)
	if resp.Error == "" {
		t.Error("expected error from config.set (not yet supported)")
	}
}

func TestConfigKeys(t *testing.T) {
	cfg := &config.Config{}
	cr := NewConfigRouter(cfg)

	req := &Request{Command: "keys", Args: map[string]string{}}
	resp := cr.Router().Dispatch(context.Background(), req)
	if resp.Error != "" {
		t.Errorf("unexpected error: %s", resp.Error)
	}
	if resp.Response == "" {
		t.Error("expected non-empty response listing keys")
	}

	// Verify all expected keys are present
	keys := strings.Fields(resp.Response)
	expected := []string{
		"arch", "base-image-path", "base-image-url", "cloud-init-iso",
		"cpus", "disk-size-gib", "gui", "hostname",
		"local-api-port", "local-ssh-port", "log-path", "memory-gib",
		"name", "network-mode", "pid", "ssh-config-path", "ssh-private-key-path",
		"ssh-user", "state-dir", "vm-dir",
	}
	if len(keys) != len(expected) {
		t.Errorf("got %d keys, want %d", len(keys), len(expected))
	}
	for i, k := range keys {
		if i < len(expected) && k != expected[i] {
			t.Errorf("key[%d] = %q, want %q", i, k, expected[i])
		}
	}
}
