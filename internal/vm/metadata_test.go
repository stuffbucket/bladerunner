//go:build darwin

package vm

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/config"
)

// TestLoadOrCreateMetadata_GeneratesBreakGlassPassword verifies a fresh VM gets a
// non-empty, sufficiently-long per-instance break-glass password persisted to the
// metadata file, and that the file is written owner-only (0600) because it now
// carries that secret.
func TestLoadOrCreateMetadata_GeneratesBreakGlassPassword(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := &config.Config{MetadataPath: filepath.Join(dir, "runtime-metadata.json")}

	md, err := loadOrCreateMetadata(cfg)
	if err != nil {
		t.Fatalf("loadOrCreateMetadata: %v", err)
	}
	if md.SSHBreakGlassPassword == "" {
		t.Fatal("break-glass password was not generated")
	}
	if len(md.SSHBreakGlassPassword) != breakGlassPasswordLen {
		t.Errorf("break-glass password length = %d, want %d", len(md.SSHBreakGlassPassword), breakGlassPasswordLen)
	}
	if md.MACAddress == "" {
		t.Error("MAC address was not generated")
	}

	info, err := os.Stat(cfg.MetadataPath)
	if err != nil {
		t.Fatalf("stat metadata: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("metadata file mode = %o, want 600 (holds the break-glass secret)", perm)
	}
}

// TestLoadOrCreateMetadata_StableAcrossCalls verifies the password is generated
// once and reused (load-or-create), so it stays stable for a VM across binary
// updates rather than rotating on every start.
func TestLoadOrCreateMetadata_StableAcrossCalls(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := &config.Config{MetadataPath: filepath.Join(dir, "runtime-metadata.json")}

	first, err := loadOrCreateMetadata(cfg)
	if err != nil {
		t.Fatalf("first loadOrCreateMetadata: %v", err)
	}
	second, err := loadOrCreateMetadata(cfg)
	if err != nil {
		t.Fatalf("second loadOrCreateMetadata: %v", err)
	}
	if first.SSHBreakGlassPassword != second.SSHBreakGlassPassword {
		t.Errorf("break-glass password changed across calls: %q -> %q", first.SSHBreakGlassPassword, second.SSHBreakGlassPassword)
	}
	if first.MACAddress != second.MACAddress {
		t.Errorf("MAC changed across calls: %q -> %q", first.MACAddress, second.MACAddress)
	}
}

// TestLoadOrCreateMetadata_BackfillsPasswordForOlderFile verifies an existing
// metadata file that predates the password field (has only a MAC) is upgraded in
// place: the MAC is preserved and a break-glass password is added and persisted.
func TestLoadOrCreateMetadata_BackfillsPasswordForOlderFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := &config.Config{MetadataPath: filepath.Join(dir, "runtime-metadata.json")}

	// Simulate a pre-existing file with only the MAC (older schema).
	const oldMAC = "02:00:00:11:22:33"
	if err := os.WriteFile(cfg.MetadataPath, []byte(`{"mac_address":"`+oldMAC+`"}`), 0o600); err != nil {
		t.Fatalf("seed metadata: %v", err)
	}

	md, err := loadOrCreateMetadata(cfg)
	if err != nil {
		t.Fatalf("loadOrCreateMetadata: %v", err)
	}
	if md.MACAddress != oldMAC {
		t.Errorf("MAC not preserved: got %q, want %q", md.MACAddress, oldMAC)
	}
	if md.SSHBreakGlassPassword == "" {
		t.Error("break-glass password was not backfilled for the older metadata file")
	}

	// The backfill must be persisted, so a later call reads the same password.
	reload, err := loadOrCreateMetadata(cfg)
	if err != nil {
		t.Fatalf("reload loadOrCreateMetadata: %v", err)
	}
	if reload.SSHBreakGlassPassword != md.SSHBreakGlassPassword {
		t.Errorf("backfilled password not persisted: %q -> %q", md.SSHBreakGlassPassword, reload.SSHBreakGlassPassword)
	}
}

// TestGenerateBreakGlassPassword_Alphabet guards that generated passwords only
// use the shell/YAML-safe alphabet, so they are safe to embed verbatim in the
// single-quoted bootstrap variable and the cloud-init chpasswd module.
func TestGenerateBreakGlassPassword_Alphabet(t *testing.T) {
	t.Parallel()
	for i := 0; i < 64; i++ {
		pw, err := generateBreakGlassPassword()
		if err != nil {
			t.Fatalf("generateBreakGlassPassword: %v", err)
		}
		if len(pw) != breakGlassPasswordLen {
			t.Fatalf("length = %d, want %d", len(pw), breakGlassPasswordLen)
		}
		for _, r := range pw {
			if !containsRune(breakGlassPasswordAlphabet, r) {
				t.Fatalf("password %q contains out-of-alphabet rune %q", pw, r)
			}
		}
	}
}

func containsRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}
