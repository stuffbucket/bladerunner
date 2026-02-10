package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/control"
)

const maxDisplayValueLen = 60

var configCmd = &cobra.Command{
	Use:   "config <get|set|keys> [key] [value]",
	Short: "Get or set configuration values",
	Long: `Manage Bladerunner configuration.

Most config values can be read without a running VM. Values that are
only available at runtime (e.g. pid, ssh-config-path) require the VM
to be started.

Examples:
  # List all config keys with their defaults and status
  br config keys

  # Get a specific config value
  br config get base-image-url

  # Set a config value (only certain keys are modifiable)
  br config set base-image-url https://cloud-images.ubuntu.com/releases/noble/release/ubuntu-24.04-server-cloudimg-arm64.img

  # List all available config keys
  br config keys`,
	Args: cobra.MinimumNArgs(1),
	RunE: runConfig,
}

func runConfig(_ *cobra.Command, args []string) error {
	subcommand := args[0]
	switch subcommand {
	case "get":
		return runConfigGet(args[1:])
	case "set":
		return runConfigSet(args[1:])
	case "keys":
		return runConfigKeys()
	default:
		return fmt.Errorf("unknown subcommand: %s (expected: get, set, or keys)", subcommand)
	}
}

// defaultConfigValue returns the default value for a config key from the
// default config. Returns empty string for runtime-only keys.
func defaultConfigValue(cfg *config.Config, k string) string {
	switch k {
	case control.ConfigKeyArch:
		return cfg.Arch
	case control.ConfigKeyBaseImageURL:
		return cfg.BaseImageURL
	case control.ConfigKeyCloudInitISO:
		return cfg.CloudInitISO
	case control.ConfigKeyCPUs:
		return strconv.FormatUint(uint64(cfg.CPUs), 10)
	case control.ConfigKeyDiskSizeGiB:
		return strconv.Itoa(cfg.DiskSizeGiB)
	case control.ConfigKeyGUI:
		return strconv.FormatBool(cfg.GUI)
	case control.ConfigKeyHostname:
		return cfg.Hostname
	case control.ConfigKeyLocalAPIPort:
		return strconv.Itoa(cfg.LocalAPIPort)
	case control.ConfigKeyLocalSSHPort:
		return strconv.Itoa(cfg.LocalSSHPort)
	case control.ConfigKeyLogPath:
		return cfg.LogPath
	case control.ConfigKeyMemoryGiB:
		return strconv.FormatUint(cfg.MemoryGiB, 10)
	case control.ConfigKeyName:
		return cfg.Name
	case control.ConfigKeyNetworkMode:
		return cfg.NetworkMode
	case control.ConfigKeySSHUser:
		return cfg.SSHUser
	case control.ConfigKeyStateDir:
		return cfg.StateDir
	case control.ConfigKeyVMDir:
		return cfg.VMDir
	default:
		return ""
	}
}

func runConfigGet(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: br config get <key>")
	}

	configKey := args[0]
	metaMap := control.ConfigKeyMetaMap()
	meta, known := metaMap[configKey]
	if !known {
		return fmt.Errorf("unknown config key: %s", configKey)
	}

	stateDir := config.DefaultStateDir()
	client := control.NewClient(stateDir)
	vmRunning := client.IsRunning()

	// If VM is running, prefer live values
	if vmRunning {
		configValue, err := client.GetConfig(configKey)
		if err == nil {
			fmt.Println(configValue)
			return nil
		}
	}

	// Runtime-only key but VM not running
	if meta.RequiresVM {
		return fmt.Errorf("%s is only available when the VM is running", configKey)
	}

	// Fall back to default config
	cfg, err := config.Default("")
	if err != nil {
		return fmt.Errorf("load defaults: %w", err)
	}

	configValue := defaultConfigValue(cfg, configKey)
	fmt.Println(configValue)
	return nil
}

func runConfigSet(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: br config set <key> <value>")
	}

	configKey := args[0]
	configValue := args[1]

	metaMap := control.ConfigKeyMetaMap()
	meta, known := metaMap[configKey]
	if !known {
		return fmt.Errorf("unknown config key: %s", configKey)
	}
	if !meta.Writable {
		return fmt.Errorf("config key %s is read-only", configKey)
	}

	stateDir := config.DefaultStateDir()
	client := control.NewClient(stateDir)

	if !client.IsRunning() {
		return fmt.Errorf("VM is not running; start it first with: %s", command("br start"))
	}

	if err := client.SetConfig(configKey, configValue); err != nil {
		return err
	}

	fmt.Printf("%s Set %s to %s\n", success("✓"), key(configKey), value(configValue))

	if meta.RequiresReset {
		fmt.Printf("\n%s This change requires a VM reset to take effect.\n", errorf("⚠"))
		fmt.Printf("  Run %s and then %s\n", command("br reset"), command("br start"))
	}

	return nil
}

func runConfigKeys() error {
	registry := control.ConfigKeyRegistry()
	cfg, err := config.Default("")
	if err != nil {
		return fmt.Errorf("load defaults: %w", err)
	}

	stateDir := config.DefaultStateDir()
	client := control.NewClient(stateDir)
	vmRunning := client.IsRunning()

	fmt.Println(title("Configuration Keys"))
	fmt.Println()

	for _, meta := range registry {
		displayValue := resolveDisplayValue(meta, cfg, client, vmRunning)
		annotation := ""
		if meta.RequiresVM && displayValue == "" {
			annotation = "requires running VM"
		}

		// Truncate long values
		if len(displayValue) > maxDisplayValueLen {
			displayValue = displayValue[:maxDisplayValueLen-3] + "..."
		}

		tags := buildTags(meta)
		printKeyLine(meta.Key, displayValue, annotation)
		printDescLine(meta.Description, tags)
	}
	fmt.Println()
	fmt.Println(subtle("Use 'br config get <key>' to see full values"))
	fmt.Println(subtle("Use 'br config set <key> <value>' to modify writable keys"))

	return nil
}

func resolveDisplayValue(meta control.ConfigKeyMeta, cfg *config.Config, client *control.Client, vmRunning bool) string {
	if vmRunning {
		if v, err := client.GetConfig(meta.Key); err == nil {
			return v
		}
	}
	if meta.RequiresVM {
		return ""
	}
	return defaultConfigValue(cfg, meta.Key)
}

func buildTags(meta control.ConfigKeyMeta) []string {
	var tags []string
	if meta.RequiresVM {
		tags = append(tags, "runtime")
	}
	if meta.RequiresReset {
		tags = append(tags, "reset required")
	}
	if meta.Writable {
		tags = append(tags, "writable")
	}
	return tags
}

func printKeyLine(k, displayValue, annotation string) {
	switch {
	case displayValue != "":
		fmt.Printf("  %s %s %s\n", key(k), subtle("="), value(displayValue))
	case annotation != "":
		fmt.Printf("  %s  %s\n", key(k), subtle(annotation))
	default:
		fmt.Printf("  %s\n", key(k))
	}
}

func printDescLine(desc string, tags []string) {
	if desc == "" && len(tags) == 0 {
		return
	}
	tagStr := strings.Join(tags, ", ")
	switch {
	case desc != "" && tagStr != "":
		desc += "  [" + tagStr + "]"
	case tagStr != "":
		desc = "[" + tagStr + "]"
	}
	fmt.Printf("    %s\n", subtle(desc))
}
