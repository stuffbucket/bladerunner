// Package disk implements bladerunner "disks": JSON manifests (".disk" files)
// that bundle an image identity, VM sizing recommendations, and a boot mode.
// A disk is resolved to a content-addressed base image, sized, and booted in an
// isolated per-disk state slot. This package owns the manifest schema, the
// builtin+user catalog, and the shared content-addressed image cache. It imports
// internal/config only (one-way; config must not import disk).
package disk

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
)

const (
	// ManifestExt is the file extension for disk manifests (content is JSON).
	ManifestExt = ".disk"

	// BootModeHeadless boots the disk without a graphical window.
	BootModeHeadless = "headless"
	// BootModeGUI boots the disk in a VZ window (the monitor turns on).
	BootModeGUI = "gui"

	// OriginBuiltin marks a disk shipped embedded in the binary.
	OriginBuiltin = "builtin"
	// OriginUser marks a disk loaded from the user's disks directory.
	OriginUser = "user"
)

// nameRe constrains a disk name: it doubles as a state-dir slot name and a
// filename, so path separators, ".", "..", whitespace, and uppercase are
// rejected. Mirrors the constraint enforced on slot identities elsewhere.
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// sha256Re matches a 64-character lowercase hex digest. Mirrors the validation
// in internal/vm.fetchSidecarSHA256.
var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Manifest is the on-disk ".disk" description of a bootable disk: image identity,
// VM sizing recommendations, and boot behavior. Serialized as JSON.
type Manifest struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Version     string `json:"version,omitempty"` // YYYY.MM.DD image build marker; tracks GuestImageVersionPath

	Image ImageSpec `json:"image"`
	VM    VMSpec    `json:"vm"`
	Boot  BootSpec  `json:"boot"`
}

// ImageSpec identifies the qcow2 to materialize. Exactly one of (per-arch URL+SHA),
// Path, or Hosted=true must resolve for the running GOARCH.
type ImageSpec struct {
	// Arches maps GOARCH ("arm64"/"amd64") to a per-arch artifact. Used when
	// Hosted is false and Path is empty.
	Arches map[string]ArchImage `json:"arches,omitempty"`
	// Path is a local qcow2/raw path; mutually exclusive with Arches/Hosted.
	Path string `json:"path,omitempty"`
	// Hosted selects the pre-baked bladerunner guest image resolved via
	// config.ResolveBaseImageURL(goarch, true). Mutually exclusive with Arches/Path.
	Hosted bool `json:"hosted,omitempty"`
}

// ArchImage is one architecture's download URL + expected digest.
type ArchImage struct {
	URL string `json:"url"`
	// SHA256 is the expected digest of the DOWNLOADED artifact (qcow2 before
	// conversion). Empty => sidecar verification fallback (see verifyImageChecksum).
	SHA256 string `json:"sha256,omitempty"`
}

// VMSpec carries sizing recommendations. These are DEFAULTS — explicit
// boot flags (--cpus/--memory/--disk) override them.
type VMSpec struct {
	CPUs        uint   `json:"cpus,omitempty"`          // default config.DefaultCPUs when 0
	MemoryGiB   uint64 `json:"memory_gib,omitempty"`    // default config.DefaultMemoryGiB when 0
	DiskSizeGiB int    `json:"disk_size_gib,omitempty"` // default config.DefaultDiskSizeGiB when 0; must be >=16 after defaulting
}

// BootSpec describes how the disk powers on.
type BootSpec struct {
	Mode      string `json:"mode"`                // "headless" | "gui"
	Autologin bool   `json:"autologin,omitempty"` // gui only; advisory (no host enforcement today)
}

// Validate checks the raw manifest invariants. It does NOT re-check numeric
// sizing (DiskSizeGiB/CPUs/MemoryGiB): those are defaulted in ApplyTo and
// authoritatively validated by config.Validate() afterwards.
func (m *Manifest) Validate() error {
	if m.Name == "" {
		return fmt.Errorf("disk name is required")
	}
	if !nameRe.MatchString(m.Name) {
		return fmt.Errorf("invalid disk name %q: must match %s (lowercase, no path separators)", m.Name, nameRe.String())
	}

	if m.Boot.Mode != BootModeHeadless && m.Boot.Mode != BootModeGUI {
		return fmt.Errorf("invalid boot mode %q: must be %q or %q", m.Boot.Mode, BootModeHeadless, BootModeGUI)
	}

	return m.Image.validate()
}

// validate enforces that exactly one image source resolves, and that any
// per-arch entries are well-formed.
func (i *ImageSpec) validate() error {
	sources := 0
	if i.Hosted {
		sources++
	}
	if i.Path != "" {
		sources++
	}
	if len(i.Arches) > 0 {
		sources++
	}
	switch {
	case sources == 0:
		return fmt.Errorf("no image source: set exactly one of image.hosted, image.path, or image.arches")
	case sources > 1:
		return fmt.Errorf("multiple image sources: set exactly one of image.hosted, image.path, or image.arches")
	}

	for arch, a := range i.Arches {
		if a.URL == "" {
			return fmt.Errorf("image.arches[%s].url is required", arch)
		}
		if a.SHA256 != "" && !sha256Re.MatchString(a.SHA256) {
			return fmt.Errorf("image.arches[%s].sha256 must be 64 lowercase hex chars", arch)
		}
	}
	return nil
}

// Parse decodes and validates a manifest from bytes (used for embedded builtins).
func Parse(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse disk: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Load reads and validates a .disk manifest from path.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("load disk %s: %w", path, err)
	}
	m, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("load disk %s: %w", path, err)
	}
	return m, nil
}
