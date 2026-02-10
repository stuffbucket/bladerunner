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
)

// Command constants
const (
	CmdPing   = "ping"
	CmdStop   = "stop"
	CmdStatus = "status"
)

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
		{Key: ConfigKeyDiskSizeGiB, RequiresReset: true, Description: "Disk size in GiB"},
		{Key: ConfigKeyGUI, RequiresReset: true, Description: "GUI console enabled"},
		{Key: ConfigKeyHostname, RequiresReset: true, Description: "VM hostname"},
		{Key: ConfigKeyLocalAPIPort, RequiresReset: true, Description: "Local API port"},
		{Key: ConfigKeyLocalSSHPort, RequiresReset: true, Description: "Local SSH port"},
		{Key: ConfigKeyLogPath, Description: "Log file path"},
		{Key: ConfigKeyMemoryGiB, RequiresReset: true, Description: "Memory in GiB"},
		{Key: ConfigKeyName, Description: "Instance name"},
		{Key: ConfigKeyNetworkMode, RequiresReset: true, Description: "Network mode (shared/bridged)"},
		{Key: ConfigKeyPID, RequiresVM: true, Description: "VM process ID"},
		{Key: ConfigKeySSHConfigPath, RequiresVM: true, Description: "SSH config file path"},
		{Key: ConfigKeySSHPrivateKeyPath, RequiresVM: true, Description: "SSH private key path"},
		{Key: ConfigKeySSHUser, Description: "SSH user"},
		{Key: ConfigKeyStateDir, Description: "State directory"},
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
