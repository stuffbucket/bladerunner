package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/control"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the status of Bladerunner VM",
	Long:  `Display the current status of the Bladerunner VM and control server.`,
	RunE:  runStatus,
}

func runStatus(_ *cobra.Command, _ []string) error {
	stateDir := config.DefaultStateDir()
	client := control.NewClient(stateDir)

	if !client.IsRunning() {
		fmt.Printf("  %s %s\n", key("VM:"), errorf("stopped"))
		fmt.Println()
		fmt.Println(subtle("Start the VM with:"), command("br start"))
		return nil
	}

	status, err := client.GetStatus()
	if err != nil {
		return fmt.Errorf("get status: %w", err)
	}

	// getConfig fetches a config value, returning empty string on error.
	getConfig := func(k string) string {
		v, err := client.GetConfig(k)
		if err != nil {
			return ""
		}
		return v
	}

	fmt.Println(title("Bladerunner Status"))
	fmt.Printf("  %s %s\n", key("VM:"), success(status))
	printIfSet("PID:", getConfig(control.ConfigKeyPID))
	printIfSet("Name:", getConfig(control.ConfigKeyName))
	printIfSet("Arch:", getConfig(control.ConfigKeyArch))
	fmt.Println()

	fmt.Println(subtle("  Resources:"))
	printIfSet("CPUs:", getConfig(control.ConfigKeyCPUs))
	printGiBIfSet("Memory:", getConfig(control.ConfigKeyMemoryGiB))
	printGiBIfSet("Disk:", getConfig(control.ConfigKeyDiskSizeGiB))
	fmt.Println()

	fmt.Println(subtle("  Image:"))
	printIfSet("URL:", getConfig(control.ConfigKeyBaseImageURL))
	printIfSet("Path:", getConfig(control.ConfigKeyBaseImagePath))
	printIfSet("Cloud-init:", getConfig(control.ConfigKeyCloudInitISO))
	fmt.Println()

	fmt.Println(subtle("  Network:"))
	sshPort := getConfig(control.ConfigKeyLocalSSHPort)
	apiPort := getConfig(control.ConfigKeyLocalAPIPort)
	if sshPort != "" {
		fmt.Printf("  %s %s\n", key("SSH:"), value("localhost:"+sshPort))
	}
	if apiPort != "" {
		fmt.Printf("  %s %s\n", key("API:"), value("localhost:"+apiPort))
	}
	printIfSet("Mode:", getConfig(control.ConfigKeyNetworkMode))
	fmt.Println()

	fmt.Println(subtle("  Access:"))
	fmt.Printf("  %s %s\n", key("Shell:"), command("br shell"))
	fmt.Printf("  %s %s\n", key("SSH:"), command("br ssh"))
	printIfSet("Log:", getConfig(control.ConfigKeyLogPath))

	return nil
}

// printIfSet prints a key-value line if the value is non-empty.
func printIfSet(label, val string) {
	if val != "" {
		fmt.Printf("  %s %s\n", key(label), value(val))
	}
}

// printGiBIfSet prints a key-value line with " GiB" suffix if the value is non-empty.
func printGiBIfSet(label, val string) {
	if val != "" {
		fmt.Printf("  %s %s GiB\n", key(label), value(val))
	}
}
