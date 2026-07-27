// Cartridge identity: the on-image format version and the layout Verify()
// checks before anything tries to boot a mounted volume.
//
// Everything in this file is plain file I/O over an already-mounted directory,
// so unlike the hdiutil-backed half of the package it is NOT gated on
// hostSupported(): a cartridge laid out on any filesystem can be inspected
// anywhere, which also keeps it unit-testable in Linux CI.

package cartridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FormatVersion is the cartridge on-image layout revision this build writes and
// is the highest it can read.
//
// It is deliberately distinct from disk.Manifest.Version, which is a GUEST
// IMAGE BUILD DATE (YYYY.MM.DD) describing the contents of root.img. This one
// describes the *shape of the cartridge* — which files exist, where, and what
// they mean — so that a cartridge packed by a future bladerunner is rejected
// with an actionable message instead of failing somewhere deep in the boot path.
//
// Bump it whenever a change to the layout would make an older br misread a
// newer cartridge.
const FormatVersion = 1

// legacyFormatVersion is the version assumed for a cartridge that carries no
// metadata file (or whose metadata omits the field). Cartridges predate
// versioning, so "absent" must mean v1, never "invalid".
const legacyFormatVersion = 1

// MetadataFile is the cartridge self-description, at the root of the volume.
const MetadataFile = "cartridge.json"

// The cartridge on-image layout. A mounted cartridge exposes a complete,
// self-contained VM: the disk manifest, the bootable root disk, EFI +
// cloud-init state, and the RW host<->guest share folder.
const (
	// ManifestFile is the packed disk.Manifest describing the VM.
	ManifestFile = "disk.json"
	// RootImageFile is the bootable raw root disk, booted in place.
	RootImageFile = "root.img"
	// StateDirName holds boot-time state (EFI vars, cloud-init seed).
	StateDirName = "state"
	// ShareDirName is the RW host<->guest VirtioFS share.
	ShareDirName = "share"
	// CloudInitDirName is the cloud-init seed directory under StateDirName.
	// It is recreated on boot when absent, so Verify does not require it.
	CloudInitDirName = "cloud-init"
	// EFIVarsFile is the EFI variable store under StateDirName. It only exists
	// after a first boot, so Verify does not require it either.
	EFIVarsFile = "efi-vars.bin"
)

// metadataFilePerm is the mode for MetadataFile: readable by anything that can
// read the cartridge, writable only by its owner.
const metadataFilePerm = 0o644

// ErrFormatTooNew matches the error returned when a cartridge's format version
// exceeds FormatVersion. Match it with errors.Is; the concrete
// *FormatVersionError carries the version numbers.
var ErrFormatTooNew = errors.New("cartridge format is newer than this bladerunner")

// ErrNotCartridge matches the error returned when a directory does not hold a
// coherent cartridge. Match it with errors.Is; the concrete *LayoutError names
// every element that is missing.
var ErrNotCartridge = errors.New("not a bladerunner cartridge")

// FormatVersionError reports a cartridge packed by a newer bladerunner than the
// one reading it.
type FormatVersionError struct {
	// Found is the format version recorded in the cartridge.
	Found int
	// Supported is the highest format version this build understands.
	Supported int
}

func (e *FormatVersionError) Error() string {
	return fmt.Sprintf(
		"cartridge was packed by a newer bladerunner (cartridge format v%d; this build supports up to v%d): upgrade br",
		e.Found, e.Supported,
	)
}

// Unwrap makes errors.Is(err, ErrFormatTooNew) work.
func (e *FormatVersionError) Unwrap() error { return ErrFormatTooNew }

// LayoutError reports a directory that is missing cartridge layout elements,
// naming each one so the user knows what is wrong rather than seeing an opaque
// boot failure.
type LayoutError struct {
	// Mountpoint is the directory that was inspected.
	Mountpoint string
	// Missing names each absent or wrong-kind element, cartridge-relative.
	Missing []string
}

func (e *LayoutError) Error() string {
	return fmt.Sprintf("%s is not a bladerunner cartridge: missing %s",
		e.Mountpoint, strings.Join(e.Missing, ", "))
}

// Unwrap makes errors.Is(err, ErrNotCartridge) work.
func (e *LayoutError) Unwrap() error { return ErrNotCartridge }

// Metadata is a cartridge's self-description, stored at MetadataFile.
type Metadata struct {
	// FormatVersion is the on-image layout revision. See FormatVersion.
	FormatVersion int `json:"format_version"`
	// Name is the cartridge/disk name it was packed under.
	Name string `json:"name,omitempty"`
	// PackedBy records the bladerunner version that packed it (provenance for
	// bug reports; never used to gate compatibility — FormatVersion is).
	PackedBy string `json:"packed_by,omitempty"`
	// PackedAt is the RFC 3339 UTC pack timestamp.
	PackedAt string `json:"packed_at,omitempty"`
}

// requiredCartridgeFiles must exist, be regular files, and be non-empty. Only
// the two elements a cartridge cannot boot without are listed: everything else
// (EFI vars, the cloud-init seed) is regenerated on boot.
var requiredCartridgeFiles = []string{ManifestFile, RootImageFile}

// requiredCartridgeDirs must exist and be directories.
var requiredCartridgeDirs = []string{StateDirName, ShareDirName}

// CheckFormatVersion validates an on-image cartridge format version against
// this build. Anything at or below FormatVersion is accepted, including 0 and
// negatives, which mean "packed before cartridges carried a version" and are
// treated as legacyFormatVersion. Only a genuinely newer format is rejected.
func CheckFormatVersion(found int) error {
	if found <= FormatVersion {
		return nil
	}
	return &FormatVersionError{Found: found, Supported: FormatVersion}
}

// ReadMetadata reads MetadataFile from the mounted cartridge at mountpoint.
//
// A cartridge with no metadata file predates format versioning and is reported
// as legacyFormatVersion with no error, so cartridges packed by earlier builds
// keep opening. A metadata file that exists but cannot be parsed IS an error:
// that is corruption, not age.
func ReadMetadata(mountpoint string) (Metadata, error) {
	path := filepath.Join(mountpoint, MetadataFile)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Metadata{FormatVersion: legacyFormatVersion}, nil
	}
	if err != nil {
		return Metadata{}, fmt.Errorf("read cartridge metadata %s: %w", path, err)
	}
	var meta Metadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		return Metadata{}, fmt.Errorf("parse cartridge metadata %s: %w", path, err)
	}
	if meta.FormatVersion < legacyFormatVersion {
		meta.FormatVersion = legacyFormatVersion
	}
	return meta, nil
}

// WriteMetadata stamps MetadataFile into the mounted cartridge at mountpoint.
// A zero meta.FormatVersion is filled in with FormatVersion (the common case:
// callers stamp the version this build writes) and a zero PackedAt with the
// current UTC time.
func WriteMetadata(mountpoint string, meta Metadata) error {
	if meta.FormatVersion == 0 {
		meta.FormatVersion = FormatVersion
	}
	if meta.PackedAt == "" {
		meta.PackedAt = time.Now().UTC().Format(time.RFC3339)
	}
	raw, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("encode cartridge metadata: %w", err)
	}
	path := filepath.Join(mountpoint, MetadataFile)
	if err := os.WriteFile(path, append(raw, '\n'), metadataFilePerm); err != nil {
		return fmt.Errorf("write cartridge metadata %s: %w", path, err)
	}
	return nil
}

// Verify checks that the directory at mountpoint holds a coherent, bootable
// cartridge and returns its metadata.
//
// It is the decision procedure for "a volume just appeared — is it something we
// can boot?", so it must be precise in both directions: a missing element is
// reported as a *LayoutError naming it (errors.Is ErrNotCartridge), and a
// cartridge from the future is reported as a *FormatVersionError telling the
// user to upgrade (errors.Is ErrFormatTooNew) rather than failing obscurely
// later in the boot path.
//
// Verify says nothing about whether the volume is mounted from a disk image —
// pair it with IsAttached for that.
func Verify(mountpoint string) (Metadata, error) {
	info, err := os.Stat(mountpoint)
	if err != nil {
		return Metadata{}, fmt.Errorf("inspect cartridge %s: %w", mountpoint, err)
	}
	if !info.IsDir() {
		return Metadata{}, &LayoutError{Mountpoint: mountpoint, Missing: []string{"a cartridge directory"}}
	}
	meta, err := ReadMetadata(mountpoint)
	if err != nil {
		return Metadata{}, err
	}
	// Version first: a future cartridge may legitimately have a layout this
	// build does not recognize, and "upgrade br" is the useful message then.
	if err := CheckFormatVersion(meta.FormatVersion); err != nil {
		return meta, err
	}
	if missing := missingLayout(mountpoint); len(missing) > 0 {
		return meta, &LayoutError{Mountpoint: mountpoint, Missing: missing}
	}
	return meta, nil
}

// IsCartridge reports whether mountpoint holds a coherent cartridge this build
// can open. It is the boolean form of Verify for call sites that only filter.
func IsCartridge(mountpoint string) bool {
	_, err := Verify(mountpoint)
	return err == nil
}

// missingLayout names every required layout element that is absent, of the
// wrong kind, or (for files) empty, in a stable order so the message is
// deterministic.
func missingLayout(mountpoint string) []string {
	missing := make([]string, 0, len(requiredCartridgeFiles)+len(requiredCartridgeDirs))
	for _, name := range requiredCartridgeFiles {
		st, err := os.Stat(filepath.Join(mountpoint, name))
		switch {
		case err != nil:
			missing = append(missing, name)
		case st.IsDir():
			missing = append(missing, name+" (is a directory, want a file)")
		case st.Size() == 0:
			missing = append(missing, name+" (empty)")
		}
	}
	for _, name := range requiredCartridgeDirs {
		st, err := os.Stat(filepath.Join(mountpoint, name))
		switch {
		case err != nil:
			missing = append(missing, name+"/")
		case !st.IsDir():
			missing = append(missing, name+"/ (is a file, want a directory)")
		}
	}
	return missing
}
