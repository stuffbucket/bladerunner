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

	// Default values for CLI flags and config
	DefaultCPUs        = 4
	DefaultMemoryGiB   = 8
	DefaultDiskSizeGiB = 64
	DefaultTimeout     = 5 * time.Minute

	// Port assignments (avoid conflicts with common services)
	DefaultLocalSSHPort = 6022
	DefaultLocalAPIPort = 18443
	DefaultVsockSSHPort = 10022
	DefaultVsockAPIPort = 18443

	// Validation constraints
	MinDiskSizeGiB     = 16
	TrustPasswordLen   = 16
	DefaultStopTimeout = 30 // seconds
)

type Config struct {
	Name              string
	Hostname          string
	StateDir          string
	VMDir             string
	DiskPath          string
	DiskSizeGiB       int
	BaseImageURL      string
	BaseImagePath     string
	MachineIDPath     string
	EFIVarsPath       string
	CloudInitISO      string
	CloudInitDir      string
	ConsoleLogPath    string
	LogPath           string
	ReportPath        string
	MetadataPath      string
	SSHUser           string
	SSHPublicKey      string
	SSHPrivateKeyPath string
	SSHConfigPath     string
	ClientCertPath    string
	ClientKeyPath     string
	TrustPassword     string
	LocalSSHPort      int
	LocalAPIPort      int
	VsockSSHPort      uint32
	VsockAPIPort      uint32
	NetworkMode       string
	BridgeInterface   string
	GUI               bool
	CPUs              uint
	MemoryGiB         uint64
	Arch              string
	WaitForIncus      time.Duration
	DashboardPath     string
}

func DefaultBaseImageURL(goarch string) (string, error) {
	switch goarch {
	case "arm64":
		return "https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-arm64.img", nil
	case "amd64":
		return "https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-amd64.img", nil
	default:
		return "", fmt.Errorf("unsupported architecture: %s", goarch)
	}
}

func Default(baseDir string) (*Config, error) {
	if baseDir == "" {
		baseDir = DefaultStateDir()
	}

	imageURL, err := DefaultBaseImageURL(runtime.GOARCH)
	if err != nil {
		return nil, err
	}

	trustPassword, err := randomHex(TrustPasswordLen)
	if err != nil {
		return nil, fmt.Errorf("generate trust password: %w", err)
	}

	cfg := &Config{
		Name:              "bladerunner",
		Hostname:          "bladerunner",
		StateDir:          baseDir,
		VMDir:             baseDir,
		DiskPath:          filepath.Join(baseDir, "disk.raw"),
		DiskSizeGiB:       DefaultDiskSizeGiB,
		BaseImageURL:      imageURL,
		BaseImagePath:     "",
		MachineIDPath:     filepath.Join(baseDir, "machine-id.bin"),
		EFIVarsPath:       filepath.Join(baseDir, "efi-vars.bin"),
		CloudInitISO:      filepath.Join(baseDir, "cloud-init.iso"),
		CloudInitDir:      filepath.Join(baseDir, "cloud-init"),
		ConsoleLogPath:    filepath.Join(baseDir, "console.log"),
		LogPath:           filepath.Join(baseDir, "bladerunner.log"),
		ReportPath:        filepath.Join(baseDir, "startup-report.json"),
		MetadataPath:      filepath.Join(baseDir, "runtime-metadata.json"),
		SSHUser:           "incus",
		SSHPublicKey:      "", // Set by EnsureSSHKeys
		SSHPrivateKeyPath: "", // Set by EnsureSSHKeys
		SSHConfigPath:     "", // Set after VM starts
		ClientCertPath:    filepath.Join(baseDir, "client.crt"),
		ClientKeyPath:     filepath.Join(baseDir, "client.key"),
		TrustPassword:     trustPassword,
		LocalSSHPort:      DefaultLocalSSHPort,
		LocalAPIPort:      DefaultLocalAPIPort,
		VsockSSHPort:      DefaultVsockSSHPort,
		VsockAPIPort:      DefaultVsockAPIPort,
		NetworkMode:       NetworkModeShared,
		BridgeInterface:   "en0",
		GUI:               true,
		CPUs:              DefaultCPUs,
		MemoryGiB:         DefaultMemoryGiB,
		Arch:              runtime.GOARCH,
		WaitForIncus:      DefaultTimeout,
		DashboardPath:     "/ui/",
	}

	return cfg, nil
}

func (c *Config) Validate() error {
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
	if c.NetworkMode != NetworkModeShared && c.NetworkMode != NetworkModeBridged {
		return fmt.Errorf("invalid network mode: %s", c.NetworkMode)
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
	if c.LocalSSHPort < 1 || c.LocalSSHPort > 65535 {
		return errors.New("local ssh port must be in range 1-65535")
	}
	if c.LocalAPIPort < 1 || c.LocalAPIPort > 65535 {
		return errors.New("local api port must be in range 1-65535")
	}
	if c.LocalSSHPort == c.LocalAPIPort {
		return errors.New("local ssh and api ports must differ")
	}
	if c.VsockSSHPort == c.VsockAPIPort {
		return errors.New("guest vsock ssh and api ports must differ")
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
		return filepath.Join(xdg, "bladerunner")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".local", "state", "bladerunner")
	}
	return filepath.Join(home, ".local", "state", "bladerunner")
}

// DefaultAptMirrorURI returns a fast Ubuntu apt mirror URI appropriate for the
// given architecture. arm64 packages live under ubuntu-ports, not ubuntu.
func DefaultAptMirrorURI(goarch string) string {
	switch goarch {
	case "arm64":
		return "http://mirrors.ocf.berkeley.edu/ubuntu-ports/"
	default:
		return "http://mirrors.ocf.berkeley.edu/ubuntu/"
	}
}
