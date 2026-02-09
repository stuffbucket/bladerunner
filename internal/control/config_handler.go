package control

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/stuffbucket/bladerunner/internal/config"
)

// configEntry defines a config key with its getter and whether it is deferred
// (i.e., empty string means "not yet available" rather than a valid value).
type configEntry struct {
	getter   func() string
	deferred bool
}

// ConfigRouter provides synchronized access to config values via the control protocol.
// The cfg pointer is captured by reference; handlers see values set after creation.
// Callers must hold Lock when mutating cfg fields that the handler reads.
type ConfigRouter struct {
	mu      sync.RWMutex
	entries map[string]configEntry
	router  *Router
}

// NewConfigRouter creates a ConfigRouter for config.get / config.set commands.
func NewConfigRouter(cfg *config.Config) *ConfigRouter {
	cr := &ConfigRouter{
		entries: map[string]configEntry{
			ConfigKeySSHConfigPath:     {getter: func() string { return cfg.SSHConfigPath }, deferred: true},
			ConfigKeySSHUser:           {getter: func() string { return cfg.SSHUser }},
			ConfigKeySSHPrivateKeyPath: {getter: func() string { return cfg.SSHPrivateKeyPath }, deferred: true},
			ConfigKeyLocalSSHPort:      {getter: func() string { return strconv.Itoa(cfg.LocalSSHPort) }},
			ConfigKeyLocalAPIPort:      {getter: func() string { return strconv.Itoa(cfg.LocalAPIPort) }},
			ConfigKeyName:              {getter: func() string { return cfg.Name }},
			ConfigKeyVMDir:             {getter: func() string { return cfg.VMDir }},
			ConfigKeyStateDir:          {getter: func() string { return cfg.StateDir }},
			ConfigKeyCPUs:              {getter: func() string { return strconv.FormatUint(uint64(cfg.CPUs), 10) }},
			ConfigKeyMemoryGiB:         {getter: func() string { return strconv.FormatUint(cfg.MemoryGiB, 10) }},
			ConfigKeyDiskSizeGiB:       {getter: func() string { return strconv.Itoa(cfg.DiskSizeGiB) }},
			ConfigKeyArch:              {getter: func() string { return cfg.Arch }},
			ConfigKeyHostname:          {getter: func() string { return cfg.Hostname }},
			ConfigKeyNetworkMode:       {getter: func() string { return cfg.NetworkMode }},
			ConfigKeyLogPath:           {getter: func() string { return cfg.LogPath }},
			ConfigKeyGUI:               {getter: func() string { return strconv.FormatBool(cfg.GUI) }},
			ConfigKeyPID:               {getter: func() string { return strconv.Itoa(os.Getpid()) }},
			ConfigKeyBaseImageURL:      {getter: func() string { return cfg.BaseImageURL }},
			ConfigKeyBaseImagePath:     {getter: func() string { return cfg.BaseImagePath }, deferred: true},
			ConfigKeyCloudInitISO:      {getter: func() string { return cfg.CloudInitISO }},
		},
		router: NewRouter(),
	}

	cr.router.HandleFunc("get", cr.handleGet)
	cr.router.HandleFunc("set", cr.handleSet)
	cr.router.HandleFunc("keys", cr.handleKeys)

	return cr
}

// Router returns the underlying Router for mounting.
func (cr *ConfigRouter) Router() *Router { return cr.router }

// Lock acquires the write lock. Hold this when mutating config fields.
func (cr *ConfigRouter) Lock() { cr.mu.Lock() }

// Unlock releases the write lock.
func (cr *ConfigRouter) Unlock() { cr.mu.Unlock() }

func (cr *ConfigRouter) handleGet(_ context.Context, req *Request) *Message {
	key := req.Args["0"]
	if key == "" {
		return &Message{Error: "usage: config.get <key>"}
	}

	entry, ok := cr.entries[key]
	if !ok {
		return &Message{Error: fmt.Sprintf("unknown config key: %s", key)}
	}

	cr.mu.RLock()
	val := entry.getter()
	cr.mu.RUnlock()

	if val == "" && entry.deferred {
		return &Message{Error: fmt.Sprintf("%s not available (VM may not have started yet)", key)}
	}
	return &Message{Response: val}
}

func (cr *ConfigRouter) handleSet(_ context.Context, _ *Request) *Message {
	return &Message{Error: "config.set is not yet supported"}
}

func (cr *ConfigRouter) handleKeys(_ context.Context, _ *Request) *Message {
	keys := make([]string, 0, len(cr.entries))
	for k := range cr.entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return &Message{Response: strings.Join(keys, " ")}
}
