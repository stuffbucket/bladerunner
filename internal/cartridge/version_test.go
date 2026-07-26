package cartridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cartridgeDirPerm mirrors the mode the pack path creates cartridge dirs with.
const cartridgeDirPerm = 0o755

// cartridgeFilePerm is the mode for the fixture files written below.
const cartridgeFilePerm = 0o644

// layoutCartridgeFixture builds a minimally complete cartridge under a fresh
// temp dir: a non-empty disk.json and root.img, plus the state/ (with its
// cloud-init/ seed dir) and share/ directories.
func layoutCartridgeFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{
		StateDirName,
		filepath.Join(StateDirName, CloudInitDirName),
		ShareDirName,
	} {
		if err := os.MkdirAll(filepath.Join(dir, sub), cartridgeDirPerm); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	writeFixtureFile(t, filepath.Join(dir, ManifestFile), `{"name":"demo"}`)
	writeFixtureFile(t, filepath.Join(dir, RootImageFile), "not-really-a-disk")
	return dir
}

func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), cartridgeFilePerm); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// writeFormatVersion stamps a raw cartridge.json carrying just format_version,
// so the compatibility matrix can express versions this build would never emit.
func writeFormatVersion(t *testing.T, dir string, version int) {
	t.Helper()
	writeFixtureFile(t, filepath.Join(dir, MetadataFile), fmt.Sprintf(`{"format_version": %d}`, version))
}

// --- format version ------------------------------------------------------

func TestCheckFormatVersion(t *testing.T) {
	tests := []struct {
		name    string
		found   int
		wantErr bool
	}{
		{name: "unversioned legacy cartridge", found: 0},
		{name: "current", found: FormatVersion},
		{name: "negative is clamped, not rejected", found: -3},
		{name: "one ahead is rejected", found: FormatVersion + 1, wantErr: true},
		{name: "far ahead is rejected", found: FormatVersion + 99, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckFormatVersion(tc.found)
			if tc.wantErr == (err == nil) {
				t.Fatalf("CheckFormatVersion(%d) = %v, wantErr=%v", tc.found, err, tc.wantErr)
			}
			if !tc.wantErr {
				return
			}
			if !errors.Is(err, ErrFormatTooNew) {
				t.Fatalf("error does not match ErrFormatTooNew: %v", err)
			}
			var fve *FormatVersionError
			if !errors.As(err, &fve) {
				t.Fatalf("error is not a *FormatVersionError: %v", err)
			}
			if fve.Found != tc.found || fve.Supported != FormatVersion {
				t.Fatalf("FormatVersionError = %+v, want Found=%d Supported=%d", fve, tc.found, FormatVersion)
			}
			// The message must tell the user what to DO, not just that it failed.
			msg := err.Error()
			for _, want := range []string{"newer bladerunner", "upgrade br", fmt.Sprintf("v%d", tc.found)} {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q missing %q", msg, want)
				}
			}
		})
	}
}

// TestFormatVersionIsNotManifestVersion guards the distinction that motivated a
// separate version: disk.Manifest.Version is a YYYY.MM.DD guest-image build
// date, so the cartridge format version must stay a small monotonic integer and
// must never be sourced from it.
func TestFormatVersionIsNotManifestVersion(t *testing.T) {
	if FormatVersion < 1 {
		t.Fatalf("FormatVersion = %d, want >= 1", FormatVersion)
	}
	if FormatVersion > 1000 {
		t.Fatalf("FormatVersion = %d looks like a date, not a format revision", FormatVersion)
	}
}

func TestReadMetadataMissingFileIsVersionOne(t *testing.T) {
	dir := t.TempDir()
	meta, err := ReadMetadata(dir)
	if err != nil {
		t.Fatalf("ReadMetadata on a cartridge with no metadata must succeed, got: %v", err)
	}
	if meta.FormatVersion != legacyFormatVersion {
		t.Fatalf("FormatVersion = %d, want %d", meta.FormatVersion, legacyFormatVersion)
	}
}

func TestReadMetadataOmittedVersionIsVersionOne(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFile(t, filepath.Join(dir, MetadataFile), `{"name":"demo"}`)
	meta, err := ReadMetadata(dir)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if meta.FormatVersion != legacyFormatVersion {
		t.Fatalf("FormatVersion = %d, want %d", meta.FormatVersion, legacyFormatVersion)
	}
	if meta.Name != "demo" {
		t.Fatalf("Name = %q, want demo", meta.Name)
	}
}

// TestReadMetadataCorruptIsAnError separates "old" from "broken": an absent
// file means a pre-versioning cartridge, but an unparseable one is corruption
// and must not be silently treated as v1.
func TestReadMetadataCorruptIsAnError(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFile(t, filepath.Join(dir, MetadataFile), "{not json")
	if _, err := ReadMetadata(dir); err == nil {
		t.Fatal("expected an error for corrupt metadata")
	}
}

func TestWriteMetadataRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := WriteMetadata(dir, Metadata{Name: "demo", PackedBy: "br-test"}); err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}
	meta, err := ReadMetadata(dir)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if meta.FormatVersion != FormatVersion {
		t.Errorf("FormatVersion = %d, want %d", meta.FormatVersion, FormatVersion)
	}
	if meta.Name != "demo" || meta.PackedBy != "br-test" {
		t.Errorf("metadata = %+v", meta)
	}
	if meta.PackedAt == "" {
		t.Error("PackedAt was not stamped")
	}

	// The on-disk form must be plain, readable JSON: a cartridge is a
	// transportable artifact people will inspect by hand.
	raw, err := os.ReadFile(filepath.Join(dir, MetadataFile))
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("metadata is not valid JSON: %v", err)
	}
	if _, ok := probe["format_version"]; !ok {
		t.Errorf("metadata has no format_version key: %s", raw)
	}
}

func TestWriteMetadataHonoursExplicitVersion(t *testing.T) {
	dir := t.TempDir()
	if err := WriteMetadata(dir, Metadata{FormatVersion: FormatVersion + 1}); err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}
	meta, err := ReadMetadata(dir)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if meta.FormatVersion != FormatVersion+1 {
		t.Fatalf("FormatVersion = %d, want %d", meta.FormatVersion, FormatVersion+1)
	}
}

// --- Verify --------------------------------------------------------------

func TestVerifyAcceptsCompleteCartridge(t *testing.T) {
	dir := layoutCartridgeFixture(t)

	// No metadata file at all: a cartridge packed before versioning existed.
	meta, err := Verify(dir)
	if err != nil {
		t.Fatalf("Verify on an unversioned cartridge: %v", err)
	}
	if meta.FormatVersion != legacyFormatVersion {
		t.Errorf("FormatVersion = %d, want %d", meta.FormatVersion, legacyFormatVersion)
	}
	if !IsCartridge(dir) {
		t.Error("IsCartridge = false for a complete cartridge")
	}

	// Now stamp the current version: still accepted.
	if err := WriteMetadata(dir, Metadata{Name: "demo"}); err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}
	meta, err = Verify(dir)
	if err != nil {
		t.Fatalf("Verify on a current cartridge: %v", err)
	}
	if meta.FormatVersion != FormatVersion || meta.Name != "demo" {
		t.Errorf("metadata = %+v", meta)
	}
}

// TestVerifyRejectsFutureFormat proves the version check runs BEFORE the layout
// check: a future cartridge whose layout this build cannot recognize must be
// reported as "upgrade br", not as a pile of missing files.
func TestVerifyRejectsFutureFormat(t *testing.T) {
	dir := t.TempDir()
	writeFormatVersion(t, dir, FormatVersion+1)

	_, err := Verify(dir)
	if err == nil {
		t.Fatal("expected a future-format cartridge to be rejected")
	}
	if !errors.Is(err, ErrFormatTooNew) {
		t.Fatalf("error does not match ErrFormatTooNew: %v", err)
	}
	if errors.Is(err, ErrNotCartridge) {
		t.Fatalf("future format must not be reported as a layout problem: %v", err)
	}
	if IsCartridge(dir) {
		t.Error("IsCartridge = true for a future-format cartridge")
	}
}

func TestVerifyNamesEachMissingElement(t *testing.T) {
	tests := []struct {
		name string
		// remove is applied to a complete fixture; each entry is a
		// cartridge-relative path.
		remove []string
		// wantNamed are substrings that must appear in the error, one per
		// missing element.
		wantNamed []string
	}{
		{
			name:      "missing manifest",
			remove:    []string{ManifestFile},
			wantNamed: []string{ManifestFile},
		},
		{
			name:      "missing root image",
			remove:    []string{RootImageFile},
			wantNamed: []string{RootImageFile},
		},
		{
			name:      "missing state dir",
			remove:    []string{StateDirName},
			wantNamed: []string{StateDirName + "/"},
		},
		{
			name:      "missing share dir",
			remove:    []string{ShareDirName},
			wantNamed: []string{ShareDirName + "/"},
		},
		{
			name:      "everything missing is reported together",
			remove:    []string{ManifestFile, RootImageFile, StateDirName, ShareDirName},
			wantNamed: []string{ManifestFile, RootImageFile, StateDirName + "/", ShareDirName + "/"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := layoutCartridgeFixture(t)
			for _, rel := range tc.remove {
				if err := os.RemoveAll(filepath.Join(dir, rel)); err != nil {
					t.Fatalf("remove %s: %v", rel, err)
				}
			}

			_, err := Verify(dir)
			if err == nil {
				t.Fatalf("expected Verify to fail with %v removed", tc.remove)
			}
			if !errors.Is(err, ErrNotCartridge) {
				t.Fatalf("error does not match ErrNotCartridge: %v", err)
			}
			var le *LayoutError
			if !errors.As(err, &le) {
				t.Fatalf("error is not a *LayoutError: %v", err)
			}
			if le.Mountpoint != dir {
				t.Errorf("LayoutError.Mountpoint = %q, want %q", le.Mountpoint, dir)
			}
			if len(le.Missing) != len(tc.wantNamed) {
				t.Errorf("Missing = %v, want %d entries", le.Missing, len(tc.wantNamed))
			}
			msg := err.Error()
			for _, want := range tc.wantNamed {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not name %q", msg, want)
				}
			}
			if IsCartridge(dir) {
				t.Error("IsCartridge = true for an incomplete cartridge")
			}
		})
	}
}

// TestVerifyRejectsWrongKind covers the near-miss cases a plain existence check
// would wave through: a directory where a file belongs, a file where a
// directory belongs, and a truncated (zero-byte) root image.
func TestVerifyRejectsWrongKind(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(t *testing.T, dir string)
		wantWord string
	}{
		{
			name: "root.img is a directory",
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				p := filepath.Join(dir, RootImageFile)
				if err := os.Remove(p); err != nil {
					t.Fatalf("remove: %v", err)
				}
				if err := os.MkdirAll(p, cartridgeDirPerm); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
			},
			wantWord: "is a directory",
		},
		{
			name: "share is a file",
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				p := filepath.Join(dir, ShareDirName)
				if err := os.RemoveAll(p); err != nil {
					t.Fatalf("remove: %v", err)
				}
				writeFixtureFile(t, p, "x")
			},
			wantWord: "is a file",
		},
		{
			name: "root.img is empty",
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				writeFixtureFile(t, filepath.Join(dir, RootImageFile), "")
			},
			wantWord: "empty",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := layoutCartridgeFixture(t)
			tc.mutate(t, dir)
			_, err := Verify(dir)
			if err == nil {
				t.Fatal("expected Verify to fail")
			}
			if !errors.Is(err, ErrNotCartridge) {
				t.Fatalf("error does not match ErrNotCartridge: %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantWord) {
				t.Fatalf("error %q does not explain the problem (%q)", err, tc.wantWord)
			}
		})
	}
}

func TestVerifyOnMissingPath(t *testing.T) {
	_, err := Verify(filepath.Join(t.TempDir(), "nope"))
	if err == nil {
		t.Fatal("expected an error for a missing mountpoint")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error should surface the stat failure, got: %v", err)
	}
}

func TestVerifyOnRegularFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cartridge.dmg")
	writeFixtureFile(t, p, "x")
	_, err := Verify(p)
	if err == nil {
		t.Fatal("expected an error for a regular file")
	}
	if !errors.Is(err, ErrNotCartridge) {
		t.Fatalf("error does not match ErrNotCartridge: %v", err)
	}
}
