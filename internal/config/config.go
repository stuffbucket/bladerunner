package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	NetworkModeShared  = "shared"
	NetworkModeBridged = "bridged"

	// DefaultBridgeInterface is the host interface used for bridged networking.
	DefaultBridgeInterface = "en0"

	// Default values for CLI flags and config
	DefaultCPUs        = 4
	DefaultMemoryGiB   = 8
	DefaultDiskSizeGiB = 64
	// DefaultTimeout bounds WaitForIncus. Trixie genericcloud's first-boot
	// bootstrap (apt install incus + admin init) can exceed 5m on stock M-series
	// hardware; 10m absorbs that. Dial back with --timeout. (#52)
	DefaultTimeout = 10 * time.Minute

	// Port assignments (avoid conflicts with common services)
	DefaultLocalSSHPort  = 6022
	DefaultLocalAPIPort  = 18443
	DefaultLocalWebPort  = 18444
	DefaultLocalOIDCPort = 15556
	DefaultVsockSSHPort  = 10022
	DefaultVsockAPIPort  = 18443
	DefaultVsockOIDCPort = 18556
	DefaultLocalNTPPort  = 15557
	DefaultVsockNTPPort  = 18557

	// DefaultAgentVsockPort is the host vsock port the in-guest br-agent dials
	// to receive configuration commands. The host listens on this port (CID 2
	// from inside the guest).
	DefaultAgentVsockPort = 19001

	// Default OIDC client ID and audience baked into Incus config.
	DefaultOIDCClientID = "bladerunner"
	DefaultOIDCAudience = "bladerunner"

	// DefaultShareTag is the VirtioFS device tag used for the host<->guest
	// shared folder (the cartridge RW share). The guest mounts this tag at
	// DefaultShareGuestPath. It must match the tag the guest fstab/mount unit
	// references; an empty tag is invalid for a VirtioFS device.
	DefaultShareTag = "bladerunner-share"

	// DefaultShareGuestPath is where the VirtioFS share is mounted inside the
	// guest. Documented and used by the cloud-init automount when sharing is
	// enabled.
	DefaultShareGuestPath = "/mnt/share"

	// HostedGuestImageTag is the GitHub Release tag bladerunner pulls pre-baked
	// guest images from when UseHostedGuestImage is enabled. The "latest" tag is
	// maintained as a moving pointer by the build-guest-image workflow.
	HostedGuestImageTag = "guest-image-latest"

	// GuestImageVersionPath is the in-guest file written by the build pipeline
	// containing the YYYY.MM.DD build date of the running image.
	GuestImageVersionPath = "/etc/bladerunner-image-version"

	// Supported guest architectures (GOARCH values).
	archARM64 = "arm64"
	archAMD64 = "amd64"

	// AuthMode values
	AuthModeOIDC = "oidc"
	AuthModeCert = "cert"

	// Validation constraints
	MinDiskSizeGiB     = 16
	TrustPasswordLen   = 16
	DefaultStopTimeout = 30 // seconds

	// XDG directory structure
	xdgLocalDir    = ".local"
	xdgStateSubdir = "state"
	appName        = "bladerunner"

	// File names
	diskFileName         = "disk.raw"
	machineIDFileName    = "machine-id.bin"
	efiVarsFileName      = "efi-vars.bin"
	cloudInitISOFileName = "cloud-init.iso"
	cloudInitDirName     = "cloud-init"
	consoleLogFileName   = "console.log"
	logFileName          = "bladerunner.log"
	reportFileName       = "startup-report.json"
	metadataFileName     = "runtime-metadata.json"
	savedStateFileName   = "saved-state.bin"
	clientCertFileName   = "client.crt"
	clientKeyFileName    = "client.key"
)

type Config struct {
	Name     string
	Hostname string
	StateDir string
	VMDir    string
	DiskPath string
	// SavedStatePath is where `br save` / `br upgrade` write the VZ saved
	// machine state. Defaults to <stateDir>/saved-state.bin.
	SavedStatePath string
	DiskSizeGiB    int
	BaseImageURL   string
	// BaseImageSHA512 is the expected SHA-512 of the downloaded base image. Set
	// for the pinned Debian default; empty for a custom --image-url (which falls
	// back to sidecar verification) or a local --image-path.
	BaseImageSHA512 string
	// BaseImageExpectedSHA256 is an explicit expected SHA-256 of the downloaded
	// base image artifact, set by a disk manifest's image.arches[arch].sha256.
	// Distinct from BaseImageSHA512 (the pinned Debian default) and from the
	// --image-url path (which clears verification). Empty => sidecar fallback.
	BaseImageExpectedSHA256 string
	// HostedImageFallback marks BaseImageURL as the pre-baked guest image chosen
	// as the DEFAULT (not an explicit user pin). It triggers two behaviors inside
	// EnsureBaseImage: (1) the hosted sidecar SHA-256 is verified fail-closed
	// (a missing/unreachable/mismatched sidecar is fatal, not warn-only), and
	// (2) on ANY hosted download/verify failure the resolver auto-falls-back to
	// the pinned Debian genericcloud + cloud-init path (flipping UseHostedGuestImage
	// and UseGuestAgent off) so a first run is never bricked. Only Default() and
	// Settings.ApplyTo(ImageHosted) set this; an explicit --image-url, a disk
	// manifest, or the forced-cloud-init escape hatch leave it false.
	HostedImageFallback bool
	BaseImagePath       string
	MachineIDPath       string
	EFIVarsPath         string
	CloudInitISO        string
	CloudInitDir        string
	ConsoleLogPath      string
	LogPath             string
	ReportPath          string
	MetadataPath        string
	SSHUser             string
	SSHPublicKey        string
	SSHPrivateKeyPath   string
	SSHConfigPath       string
	ClientCertPath      string
	ClientKeyPath       string
	TrustPassword       string
	LocalSSHPort        int
	LocalAPIPort        int
	LocalWebPort        int
	LocalOIDCPort       int
	VsockSSHPort        uint32
	VsockAPIPort        uint32
	VsockOIDCPort       uint32
	LocalNTPPort        int
	VsockNTPPort        uint32
	// AgentVsockPort is the host vsock port that br-agent (inside the guest)
	// dials to participate in the configuration handshake. Default 19001.
	AgentVsockPort uint32
	// UseGuestAgent enables the vsock-native guest agent handshake path.
	// When true, BuildCloudInit emits a minimal user-data form (SSH key +
	// systemctl enable br-agent) and the host runs the agent listener.
	// Defaults to true alongside UseHostedGuestImage: the pre-baked image
	// ships br-agent, so a fresh install boots via the agent handshake instead
	// of the first-boot apt bootstrap (#155). Forced off by the cloud-init
	// escape hatch (--cloud-init / BLADERUNNER_FORCE_CLOUD_INIT) and by the
	// automatic Debian fallback in EnsureBaseImage. Requires br-agent to be
	// present in the guest image (see #45) or installed via cloud-init override.
	UseGuestAgent bool
	// OIDCIssuerURL is the issuer URL advertised in discovery and tokens. It uses
	// the host provider's loopback port (LocalOIDCPort) so it resolves identically
	// from inside the VM (Incus, via the guest→host vsock bridge) and on the host
	// (the browser, direct) — which the browser authorization-code redirect needs.
	// Defaults to http://127.0.0.1:<LocalOIDCPort>.
	OIDCIssuerURL string
	// OIDCClientID is the OAuth2 client_id Incus uses (and that this provider expects).
	OIDCClientID string
	// OIDCAudience is the `aud` claim Incus verifies on issued tokens.
	OIDCAudience string
	// OIDCStateDir is where the signing key and runtime state live.
	OIDCStateDir string
	// IdentityDir is the directory of registered SSH-pubkey identity files.
	IdentityDir string
	// AuthMode selects how `runner` talks to Incus: "oidc" (default) or "cert" (legacy mTLS).
	AuthMode        string
	NetworkMode     string
	BridgeInterface string
	GUI             bool
	// UseHostedGuestImage selects the pre-baked bladerunner guest image hosted on
	// GitHub Releases (Debian Trixie + Incus + br-agent). It is the DEFAULT as of
	// #155: a fresh install pulls guest-image-latest and provisions via the agent
	// handshake, dropping the flaky first-boot apt install. When false (the
	// forced-cloud-init escape hatch, a custom --image-url, or the automatic
	// Debian fallback), BaseImageURL points at the pinned Debian Trixie
	// genericcloud image and cloud-init bootstraps Incus on first boot.
	UseHostedGuestImage bool
	CPUs                uint
	MemoryGiB           uint64
	Arch                string
	WaitForIncus        time.Duration
	DashboardPath       string
	// NestedVirtDisabled opts out of nested virtualization even when the host
	// supports it (set via --no-nested-virt). When false, bladerunner enables
	// nested virt where available so the guest's Incus can run VMs.
	NestedVirtDisabled bool
	// NestedVirt is the resolved nested-virtualization state for the running
	// VM ("enabled", "unsupported", or "disabled"), set by the runner at start
	// for status/UI reporting. Empty before the VM is configured.
	NestedVirt string
	// ShareDir is the host directory exposed to the guest over VirtioFS as a
	// read-WRITE host<->guest share (the cartridge share folder). Empty => no
	// directory-sharing device is added (no regression to plain start/boot).
	// When set, ShareTag must also be non-empty.
	ShareDir string
	// ShareTag is the VirtioFS device tag the guest mounts. Defaults to
	// DefaultShareTag. Only meaningful when ShareDir is set.
	ShareTag string
	// ShareGuestPath is where the share is mounted inside the guest. Defaults to
	// DefaultShareGuestPath. Only meaningful when ShareDir is set; set from a
	// cartridge manifest's Share.GuestPath so a non-default path actually mounts
	// there (not just reported).
	ShareGuestPath string
}

// DefaultBaseImageURL returns the default base image URL for the given GOARCH.
// As of #155 the default is the pre-baked hosted guest image (HostedGuestImageURL);
// the forced-cloud-init escape hatch and a custom --image-url resolve the pinned
// Debian genericcloud instead via DebianTrixieGenericCloudURL.
func DefaultBaseImageURL(goarch string) (string, error) {
	return ResolveBaseImageURL(goarch, DefaultUseHostedGuestImage())
}

// ForceCloudInitEnvVar, when set to a truthy value ("1", "true", "yes", "on"),
// forces the legacy Debian Trixie genericcloud + first-boot cloud-init path even
// though the pre-baked hosted image is the default (#155). It mirrors the
// --cloud-init CLI flag as a scriptable/non-interactive escape hatch.
const ForceCloudInitEnvVar = "BLADERUNNER_FORCE_CLOUD_INIT"

// ForceHostedImageEnvVar, when set to a truthy value ("1", "true", "yes", "on"),
// forces the pre-baked hosted guest image + agent path. It mirrors the
// --hosted-image CLI flag and completes the override surface symmetric to
// ForceCloudInitEnvVar: it lets a caller (e.g. the e2e boot-verify) deterministic-
// ally select the pre-baked image on ANY branch regardless of the built-in
// default. If both force envs are truthy, hosted wins here (the CLI rejects the
// combination up front; this only guards against a lower-level double-set).
const ForceHostedImageEnvVar = "BLADERUNNER_FORCE_HOSTED_IMAGE"

// envTruthy reports whether the named env var is set to a truthy token.
func envTruthy(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// ForceCloudInit reports whether the forced-cloud-init escape hatch is set via
// the ForceCloudInitEnvVar environment variable.
func ForceCloudInit() bool {
	return envTruthy(ForceCloudInitEnvVar)
}

// ForceHostedImage reports whether the force-hosted-image override is set via the
// ForceHostedImageEnvVar environment variable.
func ForceHostedImage() bool {
	return envTruthy(ForceHostedImageEnvVar)
}

// DefaultUseHostedGuestImage reports whether a fresh install should use the
// pre-baked hosted guest image. It is true (#155) unless the forced-cloud-init
// escape hatch is engaged (and force-hosted is not also set, which wins). This is
// the single source of truth for the built-in default that both Default() and
// DefaultSettings() consult.
func DefaultUseHostedGuestImage() bool {
	if ForceHostedImage() {
		return true
	}
	return !ForceCloudInit()
}

// DebianTrixieBuild pins the Debian Trixie genericcloud snapshot bladerunner
// downloads by default, instead of the rolling "latest" pointer, so the base
// image is reproducible and verifiable. To adopt a newer snapshot, bump this
// and the SHA-512 constants below together (from that build's SHA512SUMS).
// Source: https://cloud.debian.org/images/cloud/trixie/
const DebianTrixieBuild = "20260525-2489"

// Expected SHA-512 of the pinned genericcloud qcow2 for each arch, copied from
// the pinned build's SHA512SUMS. verifyImageChecksum checks the download
// against these (fatal on mismatch) so a pinned image is reproducible.
const (
	debianTrixieSHA512ARM64 = "b4f9240559da2c044953418d0632cee4d45e3d447a0ec6a9129ef7946e39ec4135ec9e085c176f8dc77af6536d7279c03487e9aa61fd6c628fb493886e23aef5"
	debianTrixieSHA512AMD64 = "23999f64d896af10a8c12bc391856ffb2982d459c3e4c987c241cca920920c6d0fbdccab389fbb37aeecb2e21145f60d9d50bf317bdf47f7bc1388cd945aa1da"
)

// DebianTrixieGenericCloudURL returns the upstream Debian Trixie genericcloud
// qcow2 URL for the given GOARCH, pinned to DebianTrixieBuild. This is the
// fallback base image used when the pre-baked bladerunner guest image is
// unavailable or not opted into.
func DebianTrixieGenericCloudURL(goarch string) (string, error) {
	switch goarch {
	case archARM64, archAMD64:
		return fmt.Sprintf(
			"https://cloud.debian.org/images/cloud/trixie/%s/debian-13-genericcloud-%s-%s.qcow2",
			DebianTrixieBuild, goarch, DebianTrixieBuild), nil
	default:
		return "", fmt.Errorf("unsupported architecture: %s", goarch)
	}
}

// DebianTrixieGenericCloudSHA512 returns the expected SHA-512 of the pinned
// genericcloud qcow2 for the given GOARCH, or "" for an unknown arch.
func DebianTrixieGenericCloudSHA512(goarch string) string {
	switch goarch {
	case archARM64:
		return debianTrixieSHA512ARM64
	case archAMD64:
		return debianTrixieSHA512AMD64
	default:
		return ""
	}
}

// HostedGuestImageURL returns the GitHub Release URL for the pre-baked
// bladerunner guest image for the given GOARCH. The artifact is published by
// the build-guest-image GitHub Actions workflow under the HostedGuestImageTag
// release.
func HostedGuestImageURL(goarch string) (string, error) {
	switch goarch {
	case archARM64, archAMD64:
		return fmt.Sprintf(
			"https://github.com/stuffbucket/bladerunner/releases/download/%s/bladerunner-guest-%s.qcow2",
			HostedGuestImageTag, goarch), nil
	default:
		return "", fmt.Errorf("unsupported architecture: %s", goarch)
	}
}

// ResolveBaseImageURL picks between the pre-baked hosted image and the Debian
// genericcloud fallback based on the useHosted flag. This is the single
// source of truth for which URL bladerunner uses for a fresh download.
func ResolveBaseImageURL(goarch string, useHosted bool) (string, error) {
	if useHosted {
		return HostedGuestImageURL(goarch)
	}
	return DebianTrixieGenericCloudURL(goarch)
}

func Default(baseDir string) (*Config, error) {
	if baseDir == "" {
		baseDir = DefaultStateDir()
	}

	// The pre-baked hosted image + agent handshake is the default (#155); the
	// forced-cloud-init escape hatch (BLADERUNNER_FORCE_CLOUD_INIT / --cloud-init)
	// selects the legacy pinned-Debian + first-boot cloud-init path instead.
	useHosted := DefaultUseHostedGuestImage()
	imageURL, err := ResolveBaseImageURL(runtime.GOARCH, useHosted)
	if err != nil {
		return nil, err
	}
	// Only the pinned Debian default carries an embedded SHA-512; the hosted
	// image is verified fail-closed against its published .sha256 sidecar in
	// EnsureBaseImage, which also auto-falls-back to Debian on any failure.
	baseImageSHA512 := ""
	if !useHosted {
		baseImageSHA512 = DebianTrixieGenericCloudSHA512(runtime.GOARCH)
	}

	trustPassword, err := randomHex(TrustPasswordLen)
	if err != nil {
		return nil, fmt.Errorf("generate trust password: %w", err)
	}

	cfg := &Config{
		Name:                appName,
		Hostname:            appName,
		StateDir:            baseDir,
		VMDir:               baseDir,
		DiskPath:            filepath.Join(baseDir, diskFileName),
		SavedStatePath:      filepath.Join(baseDir, savedStateFileName),
		DiskSizeGiB:         DefaultDiskSizeGiB,
		BaseImageURL:        imageURL,
		BaseImageSHA512:     baseImageSHA512,
		HostedImageFallback: useHosted,
		BaseImagePath:       "",
		MachineIDPath:       filepath.Join(baseDir, machineIDFileName),
		EFIVarsPath:         filepath.Join(baseDir, efiVarsFileName),
		CloudInitISO:        filepath.Join(baseDir, cloudInitISOFileName),
		CloudInitDir:        filepath.Join(baseDir, cloudInitDirName),
		ConsoleLogPath:      filepath.Join(baseDir, consoleLogFileName),
		LogPath:             filepath.Join(baseDir, logFileName),
		ReportPath:          filepath.Join(baseDir, reportFileName),
		MetadataPath:        filepath.Join(baseDir, metadataFileName),
		SSHUser:             "incus",
		SSHPublicKey:        "", // Set by EnsureSSHKeys
		SSHPrivateKeyPath:   "", // Set by EnsureSSHKeys
		SSHConfigPath:       "", // Set after VM starts
		ClientCertPath:      filepath.Join(baseDir, clientCertFileName),
		ClientKeyPath:       filepath.Join(baseDir, clientKeyFileName),
		TrustPassword:       trustPassword,
		LocalSSHPort:        DefaultLocalSSHPort,
		LocalAPIPort:        DefaultLocalAPIPort,
		LocalWebPort:        DefaultLocalWebPort,
		LocalOIDCPort:       DefaultLocalOIDCPort,
		VsockSSHPort:        DefaultVsockSSHPort,
		VsockAPIPort:        DefaultVsockAPIPort,
		VsockOIDCPort:       DefaultVsockOIDCPort,
		LocalNTPPort:        DefaultLocalNTPPort,
		VsockNTPPort:        DefaultVsockNTPPort,
		AgentVsockPort:      DefaultAgentVsockPort,
		UseGuestAgent:       useHosted,
		OIDCIssuerURL:       fmt.Sprintf("http://127.0.0.1:%d", DefaultLocalOIDCPort),
		OIDCClientID:        DefaultOIDCClientID,
		OIDCAudience:        DefaultOIDCAudience,
		OIDCStateDir:        filepath.Join(baseDir, "oidc"),
		IdentityDir:         defaultIdentityDir(),
		AuthMode:            AuthModeOIDC,
		NetworkMode:         NetworkModeShared,
		BridgeInterface:     DefaultBridgeInterface,
		GUI:                 false, // off by default; opt in via Settings.ShowConsole or --gui
		UseHostedGuestImage: useHosted,
		CPUs:                DefaultCPUs,
		MemoryGiB:           DefaultMemoryGiB,
		Arch:                runtime.GOARCH,
		WaitForIncus:        DefaultTimeout,
		DashboardPath:       "/ui/",
	}

	return cfg, nil
}

func (c *Config) Validate() error {
	if err := c.validateRequiredFields(); err != nil {
		return err
	}
	if err := c.validateModes(); err != nil {
		return err
	}
	if err := c.validatePorts(); err != nil {
		return err
	}
	if c.DiskSizeGiB < MinDiskSizeGiB {
		return fmt.Errorf("disk size must be at least %d GiB", MinDiskSizeGiB)
	}
	if c.CPUs < 1 {
		return errors.New("cpus must be >= 1")
	}
	if c.MemoryGiB < 2 {
		return errors.New("memory must be at least 2 GiB")
	}
	if c.BaseImagePath == "" && c.BaseImageURL == "" {
		return errors.New("either base image path or base image url must be set")
	}
	if c.WaitForIncus < time.Second {
		return errors.New("wait-for-incus must be at least 1s")
	}
	if c.ShareDir != "" && c.ShareTag == "" {
		return errors.New("share tag must be set when a share directory is configured")
	}
	return nil
}

func (c *Config) validateRequiredFields() error {
	if c.Name == "" {
		return errors.New("name is required")
	}
	if c.Hostname == "" {
		return errors.New("hostname is required")
	}
	if c.VMDir == "" {
		return errors.New("vm directory is required")
	}
	if c.LogPath == "" {
		return errors.New("log path is required")
	}
	if c.SSHUser == "" {
		return errors.New("ssh user is required")
	}
	if c.SSHPublicKey == "" {
		return errors.New("ssh public key is required")
	}
	if !strings.Contains(c.SSHPublicKey, "ssh-") {
		return errors.New("ssh public key does not look valid")
	}
	return nil
}

func (c *Config) validateModes() error {
	if c.NetworkMode != NetworkModeShared && c.NetworkMode != NetworkModeBridged {
		return fmt.Errorf("invalid network mode: %s", c.NetworkMode)
	}
	if c.AuthMode != "" && c.AuthMode != AuthModeOIDC && c.AuthMode != AuthModeCert {
		return fmt.Errorf("invalid auth mode: %s", c.AuthMode)
	}
	return nil
}

func (c *Config) validatePorts() error {
	const minPort, maxPort = 1, 65535
	if c.LocalSSHPort < minPort || c.LocalSSHPort > maxPort {
		return errors.New("local ssh port must be in range 1-65535")
	}
	if c.LocalAPIPort < minPort || c.LocalAPIPort > maxPort {
		return errors.New("local api port must be in range 1-65535")
	}
	if c.LocalOIDCPort != 0 && (c.LocalOIDCPort < minPort || c.LocalOIDCPort > maxPort) {
		return errors.New("local oidc port must be in range 1-65535")
	}
	if c.LocalNTPPort != 0 && (c.LocalNTPPort < minPort || c.LocalNTPPort > maxPort) {
		return errors.New("local ntp port must be in range 1-65535")
	}
	if c.LocalSSHPort == c.LocalAPIPort {
		return errors.New("local ssh and api ports must differ")
	}
	if c.VsockSSHPort == c.VsockAPIPort {
		return errors.New("guest vsock ssh and api ports must differ")
	}
	if c.UseGuestAgent && c.AgentVsockPort == 0 {
		return errors.New("agent vsock port must be set when use-guest-agent is true")
	}
	if c.AgentVsockPort != 0 {
		switch c.AgentVsockPort {
		case c.VsockSSHPort, c.VsockAPIPort, c.VsockOIDCPort, c.VsockNTPPort:
			return errors.New("agent vsock port must differ from ssh/api/oidc/ntp vsock ports")
		}
	}
	if c.VsockNTPPort != 0 {
		switch c.VsockNTPPort {
		case c.VsockSSHPort, c.VsockAPIPort, c.VsockOIDCPort:
			return errors.New("guest vsock ntp port must differ from ssh/api/oidc vsock ports")
		}
	}
	return nil
}

// SetSSHKeys sets the SSH key paths from externally provided values.
func (c *Config) SetSSHKeys(publicKey, privateKeyPath string) {
	if c.SSHPublicKey == "" {
		c.SSHPublicKey = publicKey
	}
	if c.SSHPrivateKeyPath == "" {
		c.SSHPrivateKeyPath = privateKeyPath
	}
}

func randomHex(bytesLen int) (string, error) {
	b := make([]byte, bytesLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// DefaultStateDir returns the XDG-compliant state directory for bladerunner.
// Precedence: BLADERUNNER_STATE_DIR > XDG_STATE_HOME/bladerunner > ~/.local/state/bladerunner
func DefaultStateDir() string {
	if d := os.Getenv("BLADERUNNER_STATE_DIR"); d != "" {
		return d
	}
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, appName)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", xdgLocalDir, xdgStateSubdir, appName)
	}
	return filepath.Join(home, xdgLocalDir, xdgStateSubdir, appName)
}

// ImageCacheDir returns the shared, content-addressed base-image cache:
// <DefaultStateDir>/cache/images. The cache is shared across disks/slots (NOT
// per-VMDir), so the same qcow2 is downloaded and converted once and reused
// instantly by every slot. This is the single source of truth for the cache
// location; internal/disk wraps it.
func ImageCacheDir() string {
	return filepath.Join(DefaultStateDir(), "cache", "images")
}

// ImageCachePath returns the content-addressed slot for a given
// downloaded-artifact SHA-256: <ImageCacheDir>/<sha256>.raw (the
// post-conversion raw image).
func ImageCachePath(sha256hex string) string {
	return filepath.Join(ImageCacheDir(), sha256hex+".raw")
}

// defaultIdentityDir returns the XDG-compliant directory of registered identity .pub files.
// This mirrors internal/oidc.DefaultIdentityDir but is duplicated here to avoid an import
// cycle (config is imported by oidc).
func defaultIdentityDir() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, appName, "identities")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".config", appName, "identities")
	}
	return filepath.Join(home, ".config", appName, "identities")
}

// DefaultAptMirrorURI returns the apt mirror URI used by the default base image.
// Debian serves all architectures from a single mirror URL, so the arch argument
// is accepted for API stability but does not vary the result.
func DefaultAptMirrorURI(_ string) string {
	return "http://deb.debian.org/debian"
}
