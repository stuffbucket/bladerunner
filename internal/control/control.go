// Package control provides a VM control plane with pluggable transports and wire formats.
//
// Architecture:
//
//	Controller (domain) ─────┐
//	                         v
//	Client ─── WireFormat ─── Transport ─── Listener ─── Router ─── Handler ─── Controller
//
// Components:
//   - Controller: Domain interface defining VM operations (Stop, Status, etc.)
//   - WireFormat: Serialization format (line-based, JSON, etc.)
//   - Transport:  Connection mechanism (Unix socket, TCP, etc.)
//   - Router:     Dispatches commands to handlers
//   - Listener:   Accepts connections and coordinates components
//   - Client:     Sends commands to a listener
package control

import "context"

// Controller defines domain operations for VM lifecycle management.
// Implementations can be local (direct VM control) or remote (RPC proxy).
type Controller interface {
	// Ping checks if the controller is responsive.
	Ping(ctx context.Context) error
	// Status returns the current VM status.
	Status(ctx context.Context) (string, error)
	// Stop gracefully shuts down the VM.
	Stop(ctx context.Context) error
}

// ControllerFunc allows functions to implement single Controller methods.
// Useful for testing or composition.
type ControllerFunc struct {
	PingFn   func(ctx context.Context) error
	StatusFn func(ctx context.Context) (string, error)
	StopFn   func(ctx context.Context) error
}

// Ping implements Controller.
func (f ControllerFunc) Ping(ctx context.Context) error {
	if f.PingFn != nil {
		return f.PingFn(ctx)
	}
	return nil
}

// Status implements Controller.
func (f ControllerFunc) Status(ctx context.Context) (string, error) {
	if f.StatusFn != nil {
		return f.StatusFn(ctx)
	}
	return StatusRunning, nil
}

// Stop implements Controller.
func (f ControllerFunc) Stop(ctx context.Context) error {
	if f.StopFn != nil {
		return f.StopFn(ctx)
	}
	return nil
}

// ProtocolVersion is the current control protocol version.
// Bump this when making breaking changes to the wire format.
const ProtocolVersion = 1

// Status constants
const (
	StatusRunning = "running"
	StatusStopped = "stopped"
	// StatusUnreachable means the host process is alive and the VM is in the
	// running state, but the guest does not answer a liveness probe (e.g. the
	// guest kernel has panicked, the vsock SSH bridge is down, or the guest is
	// still booting). The host run-state alone would report StatusRunning, so
	// this exists to avoid reporting a dead guest as healthy.
	StatusUnreachable = "unreachable"
)

// Command constants
const (
	CmdPing   = "ping"
	CmdStop   = "stop"
	CmdStatus = "status"
	// CmdSave pauses the guest and writes its machine state to the server's
	// default saved-state path. With the SaveModePause argument the guest is
	// left paused (for an upgrade handoff); otherwise it is resumed (snapshot).
	// The response body is the path written.
	CmdSave = "save"
	// CmdServerVersion reports the running server's build version string, so a
	// client can detect that a newer binary should take over (runner upgrade).
	CmdServerVersion = "version"
)

// SaveModePause is the CmdSave argument that leaves the guest paused after
// saving instead of resuming it.
const SaveModePause = "pause"

// Config command constants
const (
	CmdConfigGet  = "config.get"
	CmdConfigSet  = "config.set"
	CmdConfigKeys = "config.keys"
)

// Config key constants
const (
	ConfigKeySSHConfigPath     = "ssh-config-path"
	ConfigKeySSHUser           = "ssh-user"
	ConfigKeySSHPrivateKeyPath = "ssh-private-key-path"
	ConfigKeyLocalSSHPort      = "local-ssh-port"
	ConfigKeyLocalAPIPort      = "local-api-port"
	ConfigKeyLocalOIDCPort     = "local-oidc-port"
	ConfigKeyName              = "name"
	ConfigKeyVMDir             = "vm-dir"
	ConfigKeyStateDir          = "state-dir"
	ConfigKeyCPUs              = "cpus"
	ConfigKeyMemoryGiB         = "memory-gib"
	ConfigKeyDiskSizeGiB       = "disk-size-gib"
	ConfigKeyArch              = "arch"
	ConfigKeyHostname          = "hostname"
	ConfigKeyNetworkMode       = "network-mode"
	ConfigKeyLogPath           = "log-path"
	ConfigKeyGUI               = "gui"
	ConfigKeyPID               = "pid"
	ConfigKeyBaseImageURL      = "base-image-url"
	ConfigKeyBaseImagePath     = "base-image-path"
	ConfigKeyCloudInitISO      = "cloud-init-iso"
	ConfigKeyDiskPath          = "disk-path"
	// ConfigKeyGuestImageVersion is the YYYY.MM.DD build date baked into the
	// guest image at /etc/bladerunner-image-version. Read via SSH; empty when
	// the running image was not built by scripts/build-guest-image.sh
	// (e.g. raw Debian genericcloud).
	ConfigKeyGuestImageVersion = "guest-image-version"
	// ConfigKeyUseHostedGuestImage reports whether the pre-baked bladerunner
	// guest image is the chosen base. Defaults to "false" while
	// guest-image-latest is not yet published.
	ConfigKeyUseHostedGuestImage = "use-hosted-guest-image"
	// ConfigKeyNestedVirt reports the resolved nested-virtualization state of
	// the running VM ("enabled", "unsupported", or "disabled"), i.e. whether
	// the guest's Incus can launch VMs in addition to containers.
	ConfigKeyNestedVirt = "nested-virt"
)

// ConfigKeyMeta describes a config key's properties for CLI display and access control.
type ConfigKeyMeta struct {
	// Key is the config key name.
	Key string
	// RequiresVM indicates the value is only available from a running VM.
	RequiresVM bool
	// RequiresReset indicates changing this value requires a VM reset to take effect.
	RequiresReset bool
	// Writable indicates the value can be changed via config.set.
	Writable bool
	// Description is a short human-readable description.
	Description string
}

// ConfigKeyRegistry returns metadata for all known config keys.
func ConfigKeyRegistry() []ConfigKeyMeta {
	return []ConfigKeyMeta{
		{Key: ConfigKeyArch, Description: "Host architecture"},
		{Key: ConfigKeyBaseImagePath, RequiresVM: true, Description: "Resolved base image path"},
		{Key: ConfigKeyBaseImageURL, Writable: true, RequiresReset: true, Description: "Cloud image URL"},
		{Key: ConfigKeyCloudInitISO, Description: "Cloud-init ISO path"},
		{Key: ConfigKeyCPUs, RequiresReset: true, Description: "Number of CPUs"},
		{Key: ConfigKeyDiskPath, Description: "Main disk image path"},
		{Key: ConfigKeyDiskSizeGiB, RequiresReset: true, Description: "Disk size in GiB"},
		{Key: ConfigKeyGuestImageVersion, RequiresVM: true, Description: "Pre-baked guest image build date (YYYY.MM.DD)"},
		{Key: ConfigKeyGUI, RequiresReset: true, Description: "GUI console enabled"},
		{Key: ConfigKeyHostname, RequiresReset: true, Description: "VM hostname"},
		{Key: ConfigKeyLocalAPIPort, RequiresReset: true, Description: "Local API port"},
		{Key: ConfigKeyLocalOIDCPort, RequiresReset: true, Description: "Local OIDC provider port"},
		{Key: ConfigKeyLocalSSHPort, RequiresReset: true, Description: "Local SSH port"},
		{Key: ConfigKeyLogPath, Description: "Log file path"},
		{Key: ConfigKeyMemoryGiB, RequiresReset: true, Description: "Memory in GiB"},
		{Key: ConfigKeyName, Description: "Instance name"},
		{Key: ConfigKeyNestedVirt, RequiresVM: true, Description: "Nested virtualization / Incus VM support (enabled/unsupported/disabled)"},
		{Key: ConfigKeyNetworkMode, RequiresReset: true, Description: "Network mode (shared/bridged)"},
		{Key: ConfigKeyPID, RequiresVM: true, Description: "VM process ID"},
		{Key: ConfigKeySSHConfigPath, RequiresVM: true, Description: "SSH config file path"},
		{Key: ConfigKeySSHPrivateKeyPath, RequiresVM: true, Description: "SSH private key path"},
		{Key: ConfigKeySSHUser, Description: "SSH user"},
		{Key: ConfigKeyStateDir, Description: "State directory"},
		{Key: ConfigKeyUseHostedGuestImage, Description: "Use pre-baked hosted guest image"},
		{Key: ConfigKeyVMDir, Description: "VM directory"},
	}
}

// ConfigKeyMetaMap returns the registry as a map keyed by config key name.
func ConfigKeyMetaMap() map[string]ConfigKeyMeta {
	reg := ConfigKeyRegistry()
	m := make(map[string]ConfigKeyMeta, len(reg))
	for _, meta := range reg {
		m[meta.Key] = meta
	}
	return m
}

// Response constants
const (
	RespOK   = "ok"
	RespPong = "pong"
)
