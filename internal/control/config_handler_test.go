package control

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/config"
)

// newTestConfig returns a Config rooted in a temp directory so tests never
// touch the host filesystem. Pass the result of t.TempDir() as baseDir.
func newTestConfig(t *testing.T, baseDir string) *config.Config {
	t.Helper()
	cfg, err := config.Default(baseDir)
	if err != nil {
		t.Fatalf("config.Default(%q) error = %v", baseDir, err)
	}
	// Fill in deferred fields so they look like a running VM
	cfg.SSHConfigPath = filepath.Join(baseDir, "ssh", "config")
	cfg.SSHPrivateKeyPath = filepath.Join(baseDir, "ssh", "id_ed25519")
	cfg.BaseImagePath = filepath.Join(baseDir, "ubuntu-noble-arm64.img")
	return cfg
}

// --- config.get tests ---

func TestConfigGetAllKeys(t *testing.T) {
	baseDir := t.TempDir()
	cfg := newTestConfig(t, baseDir)
	cr := NewConfigRouter(cfg)
	router := cr.Router()

	// Map of key → expected value. Every key registered in NewConfigRouter
	// must appear here so we know the getter wiring is correct.
	expected := map[string]string{
		ConfigKeyArch:              cfg.Arch,
		ConfigKeyBaseImagePath:     cfg.BaseImagePath,
		ConfigKeyBaseImageURL:      cfg.BaseImageURL,
		ConfigKeyCloudInitISO:      cfg.CloudInitISO,
		ConfigKeyCPUs:              strconv.FormatUint(uint64(cfg.CPUs), 10),
		ConfigKeyDiskSizeGiB:       strconv.Itoa(cfg.DiskSizeGiB),
		ConfigKeyGUI:               strconv.FormatBool(cfg.GUI),
		ConfigKeyHostname:          cfg.Hostname,
		ConfigKeyLocalAPIPort:      strconv.Itoa(cfg.LocalAPIPort),
		ConfigKeyLocalSSHPort:      strconv.Itoa(cfg.LocalSSHPort),
		ConfigKeyLogPath:           cfg.LogPath,
		ConfigKeyMemoryGiB:         strconv.FormatUint(cfg.MemoryGiB, 10),
		ConfigKeyName:              cfg.Name,
		ConfigKeyNetworkMode:       cfg.NetworkMode,
		ConfigKeySSHConfigPath:     cfg.SSHConfigPath,
		ConfigKeySSHPrivateKeyPath: cfg.SSHPrivateKeyPath,
		ConfigKeySSHUser:           cfg.SSHUser,
		ConfigKeyStateDir:          cfg.StateDir,
		ConfigKeyVMDir:             cfg.VMDir,
		// PID is dynamic; validated separately below
	}

	for k, want := range expected {
		t.Run("get/"+k, func(t *testing.T) {
			req := &Request{Command: "get", Args: map[string]string{"0": k}}
			resp := router.Dispatch(context.Background(), req)
			if resp.Error != "" {
				t.Fatalf("unexpected error: %s", resp.Error)
			}
			if resp.Response != want {
				t.Errorf("Response = %q, want %q", resp.Response, want)
			}
		})
	}

	t.Run("get/pid returns valid number", func(t *testing.T) {
		req := &Request{Command: "get", Args: map[string]string{"0": "pid"}}
		resp := router.Dispatch(context.Background(), req)
		if resp.Error != "" {
			t.Fatalf("unexpected error: %s", resp.Error)
		}
		pid, err := strconv.Atoi(resp.Response)
		if err != nil || pid <= 0 {
			t.Errorf("Response = %q, want valid PID", resp.Response)
		}
		if resp.Response != strconv.Itoa(os.Getpid()) {
			t.Errorf("pid = %s, want %d", resp.Response, os.Getpid())
		}
	})
}

func TestConfigGetErrors(t *testing.T) {
	cfg := newTestConfig(t, t.TempDir())
	cr := NewConfigRouter(cfg)
	router := cr.Router()

	t.Run("unknown key", func(t *testing.T) {
		req := &Request{Command: "get", Args: map[string]string{"0": "nonexistent"}}
		resp := router.Dispatch(context.Background(), req)
		if resp.Error == "" {
			t.Error("expected error for unknown key")
		}
	})

	t.Run("missing key", func(t *testing.T) {
		req := &Request{Command: "get", Args: map[string]string{}}
		resp := router.Dispatch(context.Background(), req)
		if resp.Error == "" {
			t.Error("expected error for missing key")
		}
	})
}

func TestConfigGetDeferredKeys(t *testing.T) {
	// Deferred keys return an error when their value is empty
	deferredKeys := []string{
		ConfigKeySSHConfigPath,
		ConfigKeySSHPrivateKeyPath,
		ConfigKeyBaseImagePath,
	}

	baseDir := t.TempDir()
	cfg, err := config.Default(baseDir)
	if err != nil {
		t.Fatalf("config.Default() error = %v", err)
	}
	// Leave deferred fields at their zero values (empty strings)
	cr := NewConfigRouter(cfg)
	router := cr.Router()

	for _, k := range deferredKeys {
		t.Run("deferred-empty/"+k, func(t *testing.T) {
			req := &Request{Command: "get", Args: map[string]string{"0": k}}
			resp := router.Dispatch(context.Background(), req)
			if resp.Error == "" {
				t.Errorf("expected error for deferred key %q with empty value", k)
			}
			if !strings.Contains(resp.Error, "not available") {
				t.Errorf("error = %q, want message containing 'not available'", resp.Error)
			}
		})
	}
}

func TestConfigGetLateBinding(t *testing.T) {
	// Values set on the cfg pointer after router creation should be visible.
	baseDir := t.TempDir()
	cfg, err := config.Default(baseDir)
	if err != nil {
		t.Fatalf("config.Default() error = %v", err)
	}
	cr := NewConfigRouter(cfg)

	// Simulate VM startup populating deferred values
	cr.Lock()
	cfg.SSHConfigPath = "/late/ssh/config"
	cfg.SSHPrivateKeyPath = "/late/ssh/id_ed25519"
	cfg.BaseImagePath = "/late/image.img"
	cr.Unlock()

	router := cr.Router()

	tests := map[string]string{
		ConfigKeySSHConfigPath:     "/late/ssh/config",
		ConfigKeySSHPrivateKeyPath: "/late/ssh/id_ed25519",
		ConfigKeyBaseImagePath:     "/late/image.img",
	}

	for k, want := range tests {
		t.Run("late-binding/"+k, func(t *testing.T) {
			req := &Request{Command: "get", Args: map[string]string{"0": k}}
			resp := router.Dispatch(context.Background(), req)
			if resp.Error != "" {
				t.Fatalf("unexpected error: %s", resp.Error)
			}
			if resp.Response != want {
				t.Errorf("Response = %q, want %q", resp.Response, want)
			}
		})
	}
}

// --- config.set tests ---

func TestConfigSetWritableKeys(t *testing.T) {
	baseDir := t.TempDir()
	cfg := newTestConfig(t, baseDir)
	cr := NewConfigRouter(cfg)
	router := cr.Router()

	t.Run("set base-image-url succeeds", func(t *testing.T) {
		newURL := "https://example.com/custom.img"
		req := &Request{Command: "set", Args: map[string]string{"0": ConfigKeyBaseImageURL, "1": newURL}}
		resp := router.Dispatch(context.Background(), req)
		if resp.Error != "" {
			t.Fatalf("unexpected error: %s", resp.Error)
		}
		if resp.Response != RespOK {
			t.Errorf("Response = %q, want %q", resp.Response, RespOK)
		}

		// Verify the value was actually updated
		getReq := &Request{Command: "get", Args: map[string]string{"0": ConfigKeyBaseImageURL}}
		getResp := router.Dispatch(context.Background(), getReq)
		if getResp.Response != newURL {
			t.Errorf("after set, get = %q, want %q", getResp.Response, newURL)
		}

		// Verify the underlying config struct was mutated
		if cfg.BaseImageURL != newURL {
			t.Errorf("cfg.BaseImageURL = %q, want %q", cfg.BaseImageURL, newURL)
		}
	})
}

func TestConfigSetReadOnlyKeys(t *testing.T) {
	baseDir := t.TempDir()
	cfg := newTestConfig(t, baseDir)
	cr := NewConfigRouter(cfg)
	router := cr.Router()

	// Every key except base-image-url should be read-only
	readOnlyKeys := []string{
		ConfigKeyArch,
		ConfigKeyBaseImagePath,
		ConfigKeyCloudInitISO,
		ConfigKeyCPUs,
		ConfigKeyDiskSizeGiB,
		ConfigKeyGUI,
		ConfigKeyHostname,
		ConfigKeyLocalAPIPort,
		ConfigKeyLocalSSHPort,
		ConfigKeyLogPath,
		ConfigKeyMemoryGiB,
		ConfigKeyName,
		ConfigKeyNetworkMode,
		ConfigKeyPID,
		ConfigKeySSHConfigPath,
		ConfigKeySSHPrivateKeyPath,
		ConfigKeySSHUser,
		ConfigKeyStateDir,
		ConfigKeyVMDir,
	}

	for _, k := range readOnlyKeys {
		t.Run("set-read-only/"+k, func(t *testing.T) {
			req := &Request{Command: "set", Args: map[string]string{"0": k, "1": "new-value"}}
			resp := router.Dispatch(context.Background(), req)
			if resp.Error == "" {
				t.Errorf("expected error setting read-only key %q", k)
			}
			if !strings.Contains(resp.Error, "read-only") {
				t.Errorf("error = %q, want message containing 'read-only'", resp.Error)
			}
		})
	}
}

func TestConfigSetErrors(t *testing.T) {
	cfg := newTestConfig(t, t.TempDir())
	cr := NewConfigRouter(cfg)
	router := cr.Router()

	t.Run("missing key", func(t *testing.T) {
		req := &Request{Command: "set", Args: map[string]string{}}
		resp := router.Dispatch(context.Background(), req)
		if resp.Error == "" {
			t.Error("expected error for missing key")
		}
	})

	t.Run("missing value", func(t *testing.T) {
		req := &Request{Command: "set", Args: map[string]string{"0": ConfigKeyBaseImageURL}}
		resp := router.Dispatch(context.Background(), req)
		if resp.Error == "" {
			t.Error("expected error for missing value")
		}
	})

	t.Run("unknown key", func(t *testing.T) {
		req := &Request{Command: "set", Args: map[string]string{"0": "fake-key", "1": "val"}}
		resp := router.Dispatch(context.Background(), req)
		if resp.Error == "" {
			t.Error("expected error for unknown key")
		}
	})
}

// --- config.keys tests ---

func TestConfigKeys(t *testing.T) {
	cfg := newTestConfig(t, t.TempDir())
	cr := NewConfigRouter(cfg)
	router := cr.Router()

	req := &Request{Command: "keys", Args: map[string]string{}}
	resp := router.Dispatch(context.Background(), req)
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	if resp.Response == "" {
		t.Fatal("expected non-empty response listing keys")
	}

	keys := strings.Fields(resp.Response)

	// Keys must be sorted
	for i := 1; i < len(keys); i++ {
		if keys[i] < keys[i-1] {
			t.Errorf("keys not sorted: %q appears before %q", keys[i-1], keys[i])
			break
		}
	}

	// Every key in the registry must appear in config.keys
	registry := ConfigKeyRegistry()
	keySet := make(map[string]bool, len(keys))
	for _, k := range keys {
		keySet[k] = true
	}
	for _, meta := range registry {
		if !keySet[meta.Key] {
			t.Errorf("registry key %q not returned by config.keys", meta.Key)
		}
	}

	// Every key returned by config.keys must be in the registry
	metaMap := ConfigKeyMetaMap()
	for _, k := range keys {
		if _, ok := metaMap[k]; !ok {
			t.Errorf("config.keys returned %q which is not in the registry", k)
		}
	}
}

// --- config.set roundtrip tests ---

func TestConfigSetGetRoundtrip(t *testing.T) {
	baseDir := t.TempDir()
	cfg := newTestConfig(t, baseDir)
	cr := NewConfigRouter(cfg)
	router := cr.Router()

	// Set, get, verify, set again with different value, get again
	urls := []string{
		"https://cloud-images.ubuntu.com/releases/noble/release/ubuntu-24.04-server-cloudimg-arm64.img",
		"https://example.com/other.img",
		"https://cloud-images.ubuntu.com/releases/noble/release/ubuntu-24.04-server-cloudimg-arm64-custom.img",
	}

	for i, url := range urls {
		setReq := &Request{Command: "set", Args: map[string]string{"0": ConfigKeyBaseImageURL, "1": url}}
		setResp := router.Dispatch(context.Background(), setReq)
		if setResp.Error != "" {
			t.Fatalf("set[%d] error: %s", i, setResp.Error)
		}

		getReq := &Request{Command: "get", Args: map[string]string{"0": ConfigKeyBaseImageURL}}
		getResp := router.Dispatch(context.Background(), getReq)
		if getResp.Response != url {
			t.Errorf("roundtrip[%d]: got %q, want %q", i, getResp.Response, url)
		}
	}
}

// --- Isolation test: ensure temp dirs prevent host writes ---

func TestConfigIsolation(t *testing.T) {
	baseDir := t.TempDir()
	cfg := newTestConfig(t, baseDir)

	// All paths should be rooted in the temp directory
	paths := []struct {
		name  string
		value string
	}{
		{"StateDir", cfg.StateDir},
		{"VMDir", cfg.VMDir},
		{"DiskPath", cfg.DiskPath},
		{"MachineIDPath", cfg.MachineIDPath},
		{"EFIVarsPath", cfg.EFIVarsPath},
		{"CloudInitISO", cfg.CloudInitISO},
		{"CloudInitDir", cfg.CloudInitDir},
		{"ConsoleLogPath", cfg.ConsoleLogPath},
		{"LogPath", cfg.LogPath},
		{"ReportPath", cfg.ReportPath},
		{"MetadataPath", cfg.MetadataPath},
		{"ClientCertPath", cfg.ClientCertPath},
		{"ClientKeyPath", cfg.ClientKeyPath},
	}

	for _, p := range paths {
		t.Run("isolation/"+p.name, func(t *testing.T) {
			if !strings.HasPrefix(p.value, baseDir) {
				t.Errorf("%s = %q, want prefix %q", p.name, p.value, baseDir)
			}
		})
	}
}

// --- Registry consistency tests ---

func TestConfigKeyRegistryConsistency(t *testing.T) {
	registry := ConfigKeyRegistry()
	metaMap := ConfigKeyMetaMap()

	// Registry and map should have the same count
	if len(registry) != len(metaMap) {
		t.Errorf("registry len = %d, map len = %d (duplicate keys?)", len(registry), len(metaMap))
	}

	// Every entry must have a non-empty Key and Description
	for _, meta := range registry {
		if meta.Key == "" {
			t.Error("registry entry with empty Key")
		}
		if meta.Description == "" {
			t.Errorf("registry entry %q has empty Description", meta.Key)
		}
	}

	// Registry must be sorted by Key
	for i := 1; i < len(registry); i++ {
		if registry[i].Key < registry[i-1].Key {
			t.Errorf("registry not sorted: %q appears before %q", registry[i-1].Key, registry[i].Key)
			break
		}
	}

	// No duplicate keys
	seen := make(map[string]bool)
	for _, meta := range registry {
		if seen[meta.Key] {
			t.Errorf("duplicate registry entry for key %q", meta.Key)
		}
		seen[meta.Key] = true
	}
}

func TestConfigKeyRegistryMatchesRouter(t *testing.T) {
	// Every key the router knows about must be in the registry,
	// and vice versa (except pid, which is runtime-only but still in both).
	baseDir := t.TempDir()
	cfg := newTestConfig(t, baseDir)
	cr := NewConfigRouter(cfg)
	router := cr.Router()

	// Get all keys from the router
	keysReq := &Request{Command: "keys", Args: map[string]string{}}
	keysResp := router.Dispatch(context.Background(), keysReq)
	routerKeys := strings.Fields(keysResp.Response)

	metaMap := ConfigKeyMetaMap()
	routerKeySet := make(map[string]bool, len(routerKeys))
	for _, k := range routerKeys {
		routerKeySet[k] = true
	}

	// Every registry key must be in the router
	for k := range metaMap {
		if !routerKeySet[k] {
			t.Errorf("registry key %q not found in router", k)
		}
	}

	// Every router key must be in the registry
	for _, k := range routerKeys {
		if _, ok := metaMap[k]; !ok {
			t.Errorf("router key %q not found in registry", k)
		}
	}
}

func TestConfigKeyMetaWritableMatchesSetter(t *testing.T) {
	// If a key is marked Writable in the registry, it must have a setter
	// in the router. If not Writable, its setter must be nil.
	baseDir := t.TempDir()
	cfg := newTestConfig(t, baseDir)
	cr := NewConfigRouter(cfg)
	router := cr.Router()
	metaMap := ConfigKeyMetaMap()

	for k, meta := range metaMap {
		t.Run("writable-consistency/"+k, func(t *testing.T) {
			setReq := &Request{Command: "set", Args: map[string]string{"0": k, "1": "test-value"}}
			setResp := router.Dispatch(context.Background(), setReq)

			if meta.Writable {
				if setResp.Error != "" {
					t.Errorf("key %q is marked Writable but set returned error: %s", k, setResp.Error)
				}
			} else {
				if setResp.Error == "" {
					t.Errorf("key %q is NOT marked Writable but set succeeded", k)
				}
			}
		})
	}
}
