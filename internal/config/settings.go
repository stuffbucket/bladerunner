package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// settingsFileName is the persisted user-settings document, stored host-wide
// under DefaultStateDir (NOT inside an AirDropped cartridge). It holds only the
// user-settable subset of configuration; derived paths, ports, secrets, and
// resolved runtime values live on Config and are never persisted here.
const settingsFileName = "settings.json"

// settingsSchemaVersion is bumped when the on-disk shape changes incompatibly.
// Load tolerates an absent/zero version (treated as v1) so first run and older
// files keep working.
const settingsSchemaVersion = 1

// StartPolicy is a closed enum describing when the menubar app starts the VM.
// The zero value is intentionally invalid so a missing/garbage value is caught
// by Validate rather than silently meaning "manual".
type StartPolicy string

const (
	// StartManual is the default: the VM starts only on an explicit Start
	// action. This matches bladerunner's behavior before start policies existed.
	StartManual StartPolicy = "manual"
	// StartOnLaunch auto-starts the VM when the menubar app comes up (login).
	StartOnLaunch StartPolicy = "on-launch"
	// StartOnFirstAction lazily starts the VM the first time the user invokes a
	// VM-dependent action (Web/Shell).
	StartOnFirstAction StartPolicy = "on-first-action"
)

// Valid reports whether p is one of the closed StartPolicy values.
func (p StartPolicy) Valid() bool {
	switch p {
	case StartManual, StartOnLaunch, StartOnFirstAction:
		return true
	default:
		return false
	}
}

// NetSetting is the type-safe form of Config.NetworkMode. Values match the
// NetworkModeShared/NetworkModeBridged string constants.
type NetSetting string

const (
	NetSettingShared  NetSetting = NetSetting(NetworkModeShared)
	NetSettingBridged NetSetting = NetSetting(NetworkModeBridged)
)

// Valid reports whether n is one of the closed NetSetting values.
func (n NetSetting) Valid() bool {
	switch n {
	case NetSettingShared, NetSettingBridged:
		return true
	default:
		return false
	}
}

// NestedVirtSetting is the type-safe nested-virtualization INTENT, distinct
// from Config.NestedVirt (the resolved runtime state: enabled/unsupported/
// disabled). NestedAuto enables nested virt where the host supports it.
type NestedVirtSetting string

const (
	NestedAuto     NestedVirtSetting = "auto"
	NestedDisabled NestedVirtSetting = "disabled"
)

// Valid reports whether n is one of the closed NestedVirtSetting values.
func (n NestedVirtSetting) Valid() bool {
	switch n {
	case NestedAuto, NestedDisabled:
		return true
	default:
		return false
	}
}

// ImageKind tags the base-image source union so the four ways of selecting an
// image (hosted, pinned Debian, custom URL, local path) cannot disagree the way
// the separate UseHostedGuestImage bool + BaseImageURL + BaseImagePath fields on
// Config can.
type ImageKind string

const (
	ImageHosted    ImageKind = "hosted"     // pre-baked bladerunner guest image
	ImageDebian    ImageKind = "debian"     // pinned Debian Trixie genericcloud
	ImageCustomURL ImageKind = "custom-url" // user-supplied download URL
	ImageLocalPath ImageKind = "local-path" // local qcow2/raw on disk
)

// ImageSource is a closed (tagged) union describing where the base image comes
// from. Only the field relevant to Kind is populated; Validate enforces it.
type ImageSource struct {
	Kind ImageKind `json:"kind"`
	URL  string    `json:"url,omitempty"`  // required iff Kind == ImageCustomURL
	Path string    `json:"path,omitempty"` // required iff Kind == ImageLocalPath
}

// Valid reports whether the union is internally consistent: a known Kind with
// exactly the field that Kind requires.
func (s ImageSource) Valid() bool {
	switch s.Kind {
	case ImageHosted, ImageDebian:
		return s.URL == "" && s.Path == ""
	case ImageCustomURL:
		return s.URL != "" && s.Path == ""
	case ImageLocalPath:
		return s.Path != "" && s.URL == ""
	default:
		return false
	}
}

// Duration is a JSON-friendly time.Duration that (un)marshals as a Go duration
// string ("10m", "1h30m") so settings.json stays human-editable. It also accepts
// a bare number (nanoseconds) on read for forward/backward tolerance.
type Duration time.Duration

// MarshalJSON renders the duration as a string, e.g. "10m0s".
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON accepts either a duration string ("10m") or a numeric
// nanosecond count.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch x := v.(type) {
	case string:
		parsed, err := time.ParseDuration(x)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", x, err)
		}
		*d = Duration(parsed)
		return nil
	case float64:
		*d = Duration(time.Duration(x))
		return nil
	default:
		return fmt.Errorf("invalid duration value: %v", v)
	}
}

// Settings is the persisted, user-settable subset of configuration. It is the
// type-safe source of truth that the settings screen reads/writes and that
// start-time reconciliation overlays onto a Config (see ApplyTo). Everything
// here is something a user can reasonably change; derived paths, ports, secrets,
// and resolved runtime fields are deliberately absent.
type Settings struct {
	// SchemaVersion is the on-disk format version (see settingsSchemaVersion).
	SchemaVersion int `json:"schemaVersion"`

	// StartPolicy controls when the menubar app starts the VM.
	StartPolicy StartPolicy `json:"startPolicy"`

	// Resources.
	CPUs        uint   `json:"cpus"`
	MemoryGiB   uint64 `json:"memoryGiB"`
	DiskSizeGiB int    `json:"diskSizeGiB"`

	// Network.
	NetworkMode     NetSetting `json:"networkMode"`
	BridgeInterface string     `json:"bridgeInterface,omitempty"`

	// Base image source (closed union).
	Image ImageSource `json:"image"`

	// Advanced.
	NestedVirt   NestedVirtSetting `json:"nestedVirt"`
	WaitForIncus Duration          `json:"waitForIncus"`

	// ShowConsole opens the VZ serial/framebuffer console window on start. Off by
	// default: the window freezes at the kernel hand-off (the cloud kernel logs
	// to the serial port, not the framebuffer), which reads as a hang. Boot
	// progress is on the splash + `br logs`; this is for low-level debugging.
	ShowConsole bool `json:"showConsole"`
}

// DefaultSettings returns the user-settings document that reproduces
// bladerunner's built-in defaults. It must stay in sync with Default so a fresh
// settings.json is a no-op overlay.
func DefaultSettings() Settings {
	return Settings{
		SchemaVersion:   settingsSchemaVersion,
		StartPolicy:     StartManual,
		CPUs:            DefaultCPUs,
		MemoryGiB:       DefaultMemoryGiB,
		DiskSizeGiB:     DefaultDiskSizeGiB,
		NetworkMode:     NetSettingShared,
		BridgeInterface: DefaultBridgeInterface,
		Image:           ImageSource{Kind: ImageDebian},
		NestedVirt:      NestedAuto,
		WaitForIncus:    Duration(DefaultTimeout),
		ShowConsole:     false,
	}
}

// Validate enforces the closed-union invariants and the same numeric bounds
// Config.Validate uses for the user-settable fields, so an invalid Settings is
// rejected before it can reach a Config.
func (s Settings) Validate() error {
	if !s.StartPolicy.Valid() {
		return fmt.Errorf("invalid start policy: %q", s.StartPolicy)
	}
	if !s.NetworkMode.Valid() {
		return fmt.Errorf("invalid network mode: %q", s.NetworkMode)
	}
	if s.NetworkMode == NetSettingBridged && s.BridgeInterface == "" {
		return errors.New("bridge interface must be set when network mode is bridged")
	}
	if !s.NestedVirt.Valid() {
		return fmt.Errorf("invalid nested virt mode: %q", s.NestedVirt)
	}
	if !s.Image.Valid() {
		return fmt.Errorf("invalid image source: kind=%q url=%q path=%q", s.Image.Kind, s.Image.URL, s.Image.Path)
	}
	if s.CPUs < 1 {
		return errors.New("cpus must be >= 1")
	}
	if s.MemoryGiB < 2 {
		return errors.New("memory must be at least 2 GiB")
	}
	if s.DiskSizeGiB < MinDiskSizeGiB {
		return fmt.Errorf("disk size must be at least %d GiB", MinDiskSizeGiB)
	}
	if time.Duration(s.WaitForIncus) < time.Second {
		return errors.New("wait-for-incus must be at least 1s")
	}
	return nil
}

// SettingsPath returns the settings.json location for the given state dir.
// An empty stateDir resolves to DefaultStateDir.
func SettingsPath(stateDir string) string {
	if stateDir == "" {
		stateDir = DefaultStateDir()
	}
	return filepath.Join(stateDir, settingsFileName)
}

// LoadSettings reads and validates the persisted settings from the given state
// dir. A missing file is NOT an error: it returns DefaultSettings so first run
// behaves exactly like the built-in defaults. A present-but-invalid file is an
// error (the caller decides whether to fall back).
func LoadSettings(stateDir string) (Settings, error) {
	path := SettingsPath(stateDir)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return DefaultSettings(), nil
	}
	if err != nil {
		return Settings{}, fmt.Errorf("read settings %s: %w", path, err)
	}
	// Start from defaults so fields absent in an older/partial document keep
	// their default rather than the type's zero value.
	s := DefaultSettings()
	if err := json.Unmarshal(b, &s); err != nil {
		return Settings{}, fmt.Errorf("parse settings %s: %w", path, err)
	}
	if s.SchemaVersion == 0 {
		s.SchemaVersion = settingsSchemaVersion
	}
	if err := s.Validate(); err != nil {
		return Settings{}, fmt.Errorf("invalid settings %s: %w", path, err)
	}
	return s, nil
}

// Save validates and atomically writes the settings to the given state dir
// (temp file + rename) so a concurrent reader never observes a partial write
// and a crashed writer never corrupts the document.
func (s Settings) Save(stateDir string) error {
	if err := s.Validate(); err != nil {
		return fmt.Errorf("refusing to save invalid settings: %w", err)
	}
	if stateDir == "" {
		stateDir = DefaultStateDir()
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("create state dir %s: %w", stateDir, err)
	}
	s.SchemaVersion = settingsSchemaVersion
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode settings: %w", err)
	}
	b = append(b, '\n')

	path := SettingsPath(stateDir)
	tmp, err := os.CreateTemp(stateDir, settingsFileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp settings: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before the rename succeeds.
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp settings: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp settings: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp settings: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename settings into place: %w", err)
	}
	return nil
}

// ApplyTo overlays the user-settable Settings onto cfg, translating the
// type-safe enums/union into the resolved string/bool fields the rest of the
// codebase already consumes. It leaves derived paths, ports, secrets, and other
// runtime fields untouched. Base-image RESOLUTION (URL/SHA/path population for a
// concrete arch) is deliberately NOT done here — it stays in the existing
// resolve step so vm/disk back-compat is preserved; ApplyTo only records the
// chosen source. Callers that need a runnable image must still resolve it.
func (s Settings) ApplyTo(cfg *Config) {
	if cfg == nil {
		return
	}
	cfg.CPUs = s.CPUs
	cfg.MemoryGiB = s.MemoryGiB
	cfg.DiskSizeGiB = s.DiskSizeGiB
	cfg.NetworkMode = string(s.NetworkMode)
	if s.NetworkMode == NetSettingBridged {
		cfg.BridgeInterface = s.BridgeInterface
	}
	cfg.NestedVirtDisabled = s.NestedVirt == NestedDisabled
	cfg.WaitForIncus = time.Duration(s.WaitForIncus)
	cfg.GUI = s.ShowConsole

	switch s.Image.Kind {
	case ImageHosted:
		cfg.UseHostedGuestImage = true
		cfg.BaseImagePath = ""
		// Re-resolve the download URL to the hosted release asset for this arch
		// and drop the pinned Debian SHA-512 — the hosted image is verified
		// against its own published .sha256 sidecar (fail-closed), not an
		// embedded hash. Without this, the URL/SHA left over from Default() would
		// still point at Debian even though UseHostedGuestImage is now true.
		if url, err := ResolveBaseImageURL(cfg.Arch, true); err == nil {
			cfg.BaseImageURL = url
		}
		cfg.BaseImageSHA512 = ""
	case ImageDebian:
		cfg.UseHostedGuestImage = false
		cfg.BaseImagePath = ""
	case ImageCustomURL:
		cfg.UseHostedGuestImage = false
		cfg.BaseImageURL = s.Image.URL
		// A custom URL clears the embedded checksum; verification falls back to
		// the sidecar, matching the existing --image-url semantics.
		cfg.BaseImageSHA512 = ""
		cfg.BaseImagePath = ""
	case ImageLocalPath:
		cfg.UseHostedGuestImage = false
		cfg.BaseImagePath = s.Image.Path
	}
}
