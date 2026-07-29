package config

import (
	"errors"
	"fmt"
	"net"
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
	// DefaultTimeout bounds WaitForIncus: how long a boot waits for the guest's
	// Incus API to answer AND to report this client as authorized. It is the
	// budget `--timeout` overrides, and the only one that governs that wait.
	//
	// It has to absorb a COLD first boot, which is the slow case by a wide
	// margin: on a cartridge or a fresh Debian genericcloud disk the guest runs
	// apt-get update, installs Incus and initializes it before the API exists —
	// ~3 minutes on stock M-series hardware, and more on a slow network. 10m
	// absorbs that with room to spare; a warm re-boot answers in seconds and
	// never comes near it. Dial back with --timeout. (#52)
	DefaultTimeout = 10 * time.Minute

	// LoopbackHost is the only interface bladerunner binds host-side services
	// on. Every per-instance port is host-private; the guest reaches them over
	// vsock forwarders, never over IP.
	LoopbackHost = "127.0.0.1"

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

	// Validation constraints
	MinDiskSizeGiB = 16

	// XDG directory structure
	xdgLocalDir     = ".local"
	xdgStateSubdir  = "state"
	xdgConfigSubdir = ".config"
	appName         = "bladerunner"

	// identityDirName is the DefaultConfigDir subdirectory of registered identity
	// .pub files.
	identityDirName = "identities"

	// DefaultInstanceName names the single flat instance that lives directly in
	// the default state dir. It is the instance that keeps the well-known ports
	// and the legacy "Host bladerunner" ssh alias; every other instance is named
	// after its own state directory (see Config.InstanceName).
	DefaultInstanceName = appName

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
	BaseImagePath           string
	MachineIDPath           string
	EFIVarsPath             string
	CloudInitISO            string
	CloudInitDir            string
	ConsoleLogPath          string
	LogPath                 string
	ReportPath              string
	MetadataPath            string
	SSHUser                 string
	SSHPublicKey            string
	SSHPrivateKeyPath       string
	SSHConfigPath           string
	ClientCertPath          string
	ClientKeyPath           string
	LocalSSHPort            int
	LocalAPIPort            int
	LocalWebPort            int
	LocalOIDCPort           int
	VsockSSHPort            uint32
	VsockAPIPort            uint32
	VsockOIDCPort           uint32
	LocalNTPPort            int
	VsockNTPPort            uint32
	// OIDCIssuerURL is the issuer URL advertised in discovery and tokens. It uses
	// the host provider's loopback port (LocalOIDCPort) so it resolves identically
	// from inside the VM (Incus, via the guest→host vsock bridge) and on the host
	// (the browser, direct) — which the browser authorization-code redirect needs.
	// Defaults to http://127.0.0.1:<LocalOIDCPort>. It is DERIVED: never format it
	// from a port constant, always let AssignPorts re-derive it.
	OIDCIssuerURL string
	// OIDCClientID is the OAuth2 client_id Incus uses (and that this provider expects).
	OIDCClientID string
	// OIDCAudience is the `aud` claim Incus verifies on issued tokens.
	OIDCAudience string
	// OIDCStateDir is where the signing key and runtime state live.
	OIDCStateDir string
	// IdentityDir is the directory of registered SSH-pubkey identity files.
	IdentityDir     string
	NetworkMode     string
	BridgeInterface string
	GUI             bool
	// UseHostedGuestImage selects the pre-baked bladerunner guest image hosted on
	// GitHub Releases (the guest-image-latest release). It defaults to TRUE: a
	// fresh install resolves to the pre-baked image (faster first boot, no
	// first-boot apt). When false, BaseImageURL points at the Debian Trixie
	// genericcloud image and cloud-init bootstraps Incus on first boot — the
	// warned auto-fallback and the --debian-image escape hatch both flip it back.
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
	// HostListeners holds loopback listeners bound ahead of the services that
	// use them (see the HostListeners type). Nil on a plain config, in which
	// case every service binds its own address exactly as before.
	HostListeners HostListeners
}

// DefaultBaseImageURL returns the default base image URL for the given GOARCH.
// The default is now the pre-baked hosted guest image (resolved through
// HostedGuestImageURL); the Debian genericcloud image is the warned auto-fallback
// and the --debian-image escape hatch. This mirrors Default(), which resolves the
// same hosted URL.
func DefaultBaseImageURL(goarch string) (string, error) {
	return HostedGuestImageURL(goarch)
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

// UseDebianImage repoints cfg at the pinned Debian Trixie genericcloud +
// cloud-init path for its arch: it re-resolves BaseImageURL, restores the
// embedded SHA-512 (so the download is fail-closed verified against the pinned
// hash), and disarms UseHostedGuestImage. It also clears any BaseImagePath and
// disk-manifest SHA-256 so nothing left over from the hosted default shadows the
// Debian selection. This is the single mutation shared by the --debian-image
// escape hatch and the warned auto-fallback. It errors only if the arch has no
// Debian image.
func UseDebianImage(cfg *Config) error {
	url, err := DebianTrixieGenericCloudURL(cfg.Arch)
	if err != nil {
		return err
	}
	cfg.BaseImageURL = url
	cfg.BaseImageSHA512 = DebianTrixieGenericCloudSHA512(cfg.Arch)
	cfg.BaseImageExpectedSHA256 = ""
	cfg.BaseImagePath = ""
	cfg.UseHostedGuestImage = false
	return nil
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

// ForceHostedImageEnvVar, when set to a truthy value ("1", "true", "yes", "on"),
// forces the pre-baked hosted guest image for the run — the non-interactive
// equivalent of the --hosted-image start flag. Since the hosted image is now the
// default, this mostly exists to re-select it after a persisted Settings image
// choice or to make the intent explicit; it is the mutual-exclusion counterpart
// of ForceDebianImageEnvVar.
const ForceHostedImageEnvVar = "BLADERUNNER_FORCE_HOSTED_IMAGE"

// ForceDebianImageEnvVar, when set to a truthy value ("1", "true", "yes", "on"),
// forces the Debian Trixie genericcloud + cloud-init path for the run — the
// non-interactive equivalent of the --debian-image start flag. This is the
// "bring your own generic image" escape hatch out of the pre-baked default. It is
// mutually exclusive with ForceHostedImageEnvVar / --hosted-image.
const ForceDebianImageEnvVar = "BLADERUNNER_FORCE_DEBIAN_IMAGE"

// ForceHostedImage reports whether the forced-hosted-image override is set via
// the ForceHostedImageEnvVar environment variable.
func ForceHostedImage() bool {
	return envTruthy(ForceHostedImageEnvVar)
}

// ForceDebianImage reports whether the forced-Debian-image override is set via
// the ForceDebianImageEnvVar environment variable.
func ForceDebianImage() bool {
	return envTruthy(ForceDebianImageEnvVar)
}

// envTruthy reports whether the named env var is set to a truthy value.
func envTruthy(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func Default(baseDir string) (*Config, error) {
	if baseDir == "" {
		baseDir = DefaultStateDir()
	}

	// The pre-baked (hosted) guest image is the default: faster first boot, no
	// first-boot apt. The Debian genericcloud path is the warned auto-fallback
	// (internal/vm/assets.go) and the --debian-image escape hatch.
	useHosted := true
	imageURL, err := ResolveBaseImageURL(runtime.GOARCH, useHosted)
	if err != nil {
		return nil, err
	}
	// Only the pinned Debian fallback carries an embedded checksum; the hosted
	// image is verified fail-closed against its own published .sha256 sidecar.
	baseImageSHA512 := ""
	if !useHosted {
		baseImageSHA512 = DebianTrixieGenericCloudSHA512(runtime.GOARCH)
	}

	cfg := &Config{
		Name:              appName,
		Hostname:          appName,
		StateDir:          baseDir,
		VMDir:             baseDir,
		DiskPath:          filepath.Join(baseDir, diskFileName),
		SavedStatePath:    filepath.Join(baseDir, savedStateFileName),
		DiskSizeGiB:       DefaultDiskSizeGiB,
		BaseImageURL:      imageURL,
		BaseImageSHA512:   baseImageSHA512,
		BaseImagePath:     "",
		MachineIDPath:     filepath.Join(baseDir, machineIDFileName),
		EFIVarsPath:       filepath.Join(baseDir, efiVarsFileName),
		CloudInitISO:      filepath.Join(baseDir, cloudInitISOFileName),
		CloudInitDir:      filepath.Join(baseDir, cloudInitDirName),
		ConsoleLogPath:    filepath.Join(baseDir, consoleLogFileName),
		LogPath:           filepath.Join(baseDir, logFileName),
		ReportPath:        filepath.Join(baseDir, reportFileName),
		MetadataPath:      filepath.Join(baseDir, metadataFileName),
		SSHUser:           "bladerunner",
		SSHPublicKey:      "", // Set by EnsureSSHKeys
		SSHPrivateKeyPath: "", // Set by EnsureSSHKeys
		SSHConfigPath:     "", // Set after VM starts
		ClientCertPath:    filepath.Join(baseDir, clientCertFileName),
		ClientKeyPath:     filepath.Join(baseDir, clientKeyFileName),
		// Host loopback ports (and everything derived from them) are assigned
		// below via AssignPorts; the vsock ports are per-VM namespaced and stay
		// constant.
		VsockSSHPort:        DefaultVsockSSHPort,
		VsockAPIPort:        DefaultVsockAPIPort,
		VsockOIDCPort:       DefaultVsockOIDCPort,
		VsockNTPPort:        DefaultVsockNTPPort,
		OIDCClientID:        DefaultOIDCClientID,
		OIDCAudience:        DefaultOIDCAudience,
		OIDCStateDir:        filepath.Join(baseDir, "oidc"),
		IdentityDir:         DefaultIdentityDir(),
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

	// The default instance keeps the well-known ports. Routing them through
	// AssignPorts (rather than assigning OIDCIssuerURL from a constant here)
	// means the derived URLs are produced by exactly one code path, whether the
	// ports came from these constants or from a runtime reservation.
	cfg.AssignPorts(DefaultPortAssignment())

	return cfg, nil
}

// Port reservation names. They label the members of an instance's port set
// (see internal/portalloc) and key the pre-bound listener hand-off below, so
// the allocator, the config, and the services that bind agree on one vocabulary.
const (
	// PortNameSSH is the loopback SSH forwarder port.
	PortNameSSH = "ssh"
	// PortNameAPI is the loopback Incus API forwarder port.
	PortNameAPI = "api"
	// PortNameWeb is the loopback web-UI proxy port.
	PortNameWeb = "web"
	// PortNameOIDC is the loopback OIDC provider port.
	PortNameOIDC = "oidc"
	// PortNameNTP is the loopback SNTP responder port.
	PortNameNTP = "ntp"
)

// PortAssignment is one instance's resolved set of host loopback ports.
//
// A zero OIDC or NTP port means that service is disabled (the existing
// convention); a zero SSH or API port is invalid and is rejected by Validate.
type PortAssignment struct {
	SSH  int
	API  int
	Web  int
	OIDC int
	NTP  int
}

// DefaultPortAssignment returns the well-known ports the flat default instance
// keeps, so existing docs, muscle memory, and hand-written ssh configs continue
// to work. Additional instances take whatever portalloc gives them.
func DefaultPortAssignment() PortAssignment {
	return PortAssignment{
		SSH:  DefaultLocalSSHPort,
		API:  DefaultLocalAPIPort,
		Web:  DefaultLocalWebPort,
		OIDC: DefaultLocalOIDCPort,
		NTP:  DefaultLocalNTPPort,
	}
}

// Ports returns the currently assigned host loopback ports.
func (c *Config) Ports() PortAssignment {
	return PortAssignment{
		SSH:  c.LocalSSHPort,
		API:  c.LocalAPIPort,
		Web:  c.LocalWebPort,
		OIDC: c.LocalOIDCPort,
		NTP:  c.LocalNTPPort,
	}
}

// AssignPorts writes a resolved port set onto c and re-derives every value
// built from those ports.
//
// The re-derivation is the point of this method. OIDCIssuerURL used to be
// formatted from the DefaultLocalOIDCPort constant rather than from the field,
// so moving the provider to another port produced a config that looked healthy
// and only failed at login time, long after boot reported success. Anything
// derived from a port must be recomputed here, never at the point of use.
func (c *Config) AssignPorts(p PortAssignment) {
	c.LocalSSHPort = p.SSH
	c.LocalAPIPort = p.API
	c.LocalWebPort = p.Web
	c.LocalOIDCPort = p.OIDC
	c.LocalNTPPort = p.NTP
	c.derivePortURLs()
}

// PortSource supplies resolved ports by reservation name. *portalloc.Set
// satisfies it structurally, which keeps package config free of a dependency on
// the allocator.
type PortSource interface {
	Port(name string) int
}

// AssignPortsFrom writes the ports named by src (PortNameSSH and friends) onto
// c and re-derives the values built from them, exactly as AssignPorts does.
//
// Names the source does not know — its Port returns 0 — leave the corresponding
// field untouched, so a caller that deliberately disabled OIDC or SNTP by not
// reserving a port keeps its 0, and a caller that only reserved a subset does
// not silently zero the rest. Use AssignPorts to set a port to 0 on purpose.
func (c *Config) AssignPortsFrom(src PortSource) {
	if src == nil {
		return
	}
	ports := c.Ports()
	assign := func(name string, dst *int) {
		if p := src.Port(name); p != 0 {
			*dst = p
		}
	}
	assign(PortNameSSH, &ports.SSH)
	assign(PortNameAPI, &ports.API)
	assign(PortNameWeb, &ports.Web)
	assign(PortNameOIDC, &ports.OIDC)
	assign(PortNameNTP, &ports.NTP)
	c.AssignPorts(ports)
}

// derivePortURLs recomputes every URL built from a host loopback port. It is
// the single place those URLs are formatted; Default and AssignPorts both go
// through it so the two can never disagree.
func (c *Config) derivePortURLs() {
	c.OIDCIssuerURL = OIDCIssuerURLForPort(c.LocalOIDCPort)
}

// OIDCIssuerURLForPort returns the issuer URL for a provider listening on the
// given host loopback port. The URL must resolve identically inside the guest
// (Incus, via the guest->host vsock bridge) and on the host (the browser
// following the authorization-code redirect), which is why it is loopback.
func OIDCIssuerURLForPort(port int) string {
	return fmt.Sprintf("http://%s:%d", LoopbackHost, port)
}

// LoopbackAddr formats a host loopback listen/dial address for a port.
func LoopbackAddr(port int) string {
	return fmt.Sprintf("%s:%d", LoopbackHost, port)
}

// InstanceName returns the name this instance is known by: the flat default
// instance (whose state lives directly in the default state dir) is
// DefaultInstanceName; every other instance is named after its own state
// directory — a disk slot (<state>/disks/<name>), a cartridge mountpoint, or a
// custom --state-dir. It is the ssh alias suffix and the registry key.
func (c *Config) InstanceName() string {
	dir := c.VMDir
	if dir == "" {
		dir = c.StateDir
	}
	if dir == "" || filepath.Clean(dir) == filepath.Clean(DefaultStateDir()) {
		return DefaultInstanceName
	}
	return filepath.Base(dir)
}

// HostListeners carries loopback listeners that were bound before the services
// that use them were constructed, keyed by port name.
//
// This is a runtime hand-off, not configuration: reserving a port and then
// closing it so the real service can re-bind leaves a window in which another
// process takes the port. Callers reserve with internal/portalloc, park the
// detached listeners here, and each service takes the one it needs. A missing
// entry simply means "bind the address yourself", which is what every caller
// did before per-instance ports existed.
type HostListeners map[string]net.Listener

// SetHostListener parks a pre-bound listener for the named port. It must be
// called before the VM is started; the map is not safe for concurrent use.
func (c *Config) SetHostListener(name string, ln net.Listener) {
	if ln == nil {
		return
	}
	if c.HostListeners == nil {
		c.HostListeners = make(HostListeners)
	}
	c.HostListeners[name] = ln
}

// TakeHostListener hands the named pre-bound listener to the caller, which
// becomes responsible for closing it, and removes it from the config so it can
// only be consumed once. It returns nil when no listener was parked.
func (c *Config) TakeHostListener(name string) net.Listener {
	ln, ok := c.HostListeners[name]
	if !ok {
		return nil
	}
	delete(c.HostListeners, name)
	return ln
}

// CloseHostListeners releases every listener still parked (i.e. never taken by
// a service, because start failed or that service was disabled), joining any
// close errors. Safe to call more than once.
func (c *Config) CloseHostListeners() error {
	var errs []error
	for name, ln := range c.HostListeners {
		if err := ln.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close %s listener: %w", name, err))
		}
		delete(c.HostListeners, name)
	}
	return errors.Join(errs...)
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

// DefaultConfigDir returns the XDG-compliant configuration directory for
// bladerunner.
// Precedence: XDG_CONFIG_HOME/bladerunner > ~/.config/bladerunner, falling back
// to ./.config/bladerunner when the home directory cannot be determined.
//
// This is the single source of truth for the configuration directory. Every
// other package that needs a path under it (internal/disk, internal/ssh,
// internal/oidc) builds on this helper rather than repeating the lookup.
func DefaultConfigDir() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, appName)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", xdgConfigSubdir, appName)
	}
	return filepath.Join(home, xdgConfigSubdir, appName)
}

// DefaultIdentityDir returns the XDG-compliant directory of registered identity
// .pub files. This is the single source of truth for the identity directory
// layout; internal/oidc wraps it (config is imported by oidc, so the helper
// lives here to avoid an import cycle).
func DefaultIdentityDir() string {
	return filepath.Join(DefaultConfigDir(), identityDirName)
}

// DefaultAptMirrorURI returns the apt mirror URI used by the default base image.
// Debian serves all architectures from a single mirror URL, so the arch argument
// is accepted for API stability but does not vary the result.
func DefaultAptMirrorURI(_ string) string {
	return "http://deb.debian.org/debian"
}
