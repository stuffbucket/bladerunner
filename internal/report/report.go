package report

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

type StartupReport struct {
	GeneratedAt time.Time `json:"generated_at"`
	Host        HostInfo  `json:"host"`
	VM          VMInfo    `json:"vm"`
	Network     NetInfo   `json:"network"`
	Incus       IncusInfo `json:"incus"`
	Access      Access    `json:"access"`
}

type HostInfo struct {
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	CPUCount     int    `json:"cpu_count"`
	RequestedCPU uint   `json:"requested_cpu"`
}

type VMInfo struct {
	Name          string `json:"name"`
	Hostname      string `json:"hostname"`
	Directory     string `json:"directory"`
	DiskPath      string `json:"disk_path"`
	DiskSizeGiB   int    `json:"disk_size_gib"`
	MemoryGiB     uint64 `json:"memory_gib"`
	GuestArch     string `json:"guest_arch"`
	GUIEnabled    bool   `json:"gui_enabled"`
	ConsoleLog    string `json:"console_log"`
	CloudInitISO  string `json:"cloud_init_iso"`
	BaseImageURL  string `json:"base_image_url,omitempty"`
	BaseImagePath string `json:"base_image_path,omitempty"`
}

type NetInfo struct {
	Mode             string `json:"mode"`
	BridgeInterface  string `json:"bridge_interface,omitempty"`
	MACAddress       string `json:"mac_address"`
	LocalSSHEndpoint string `json:"local_ssh_endpoint"`
	LocalAPIEndpoint string `json:"local_api_endpoint"`
	DashboardURL     string `json:"dashboard_url"`
}

type IncusInfo struct {
	ServerVersion string   `json:"server_version,omitempty"`
	APIVersion    string   `json:"api_version,omitempty"`
	Auth          string   `json:"auth,omitempty"`
	ServerName    string   `json:"server_name,omitempty"`
	Addresses     []string `json:"addresses,omitempty"`
	APIExtensions int      `json:"api_extensions"`
}

type Access struct {
	SSHCommand          string `json:"ssh_command"`
	SSHConfigPath       string `json:"ssh_config_path,omitempty"`
	SSHKeyPath          string `json:"ssh_key_path,omitempty"`
	RESTExample         string `json:"rest_example"`
	GoClientExamplePath string `json:"go_client_example_path"`
	ClientCertPath      string `json:"client_cert_path"`
	ClientKeyPath       string `json:"client_key_path"`
	LogPath             string `json:"log_path"`
}

func SaveJSON(path string, report *StartupReport) error {
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal startup report: %w", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("write startup report: %w", err)
	}
	return nil
}

func RenderText(r *StartupReport) string {
	var b strings.Builder
	b.WriteString("Bladerunner Startup Report\n")
	b.WriteString(fmt.Sprintf("Generated: %s\n", r.GeneratedAt.Format(time.RFC3339)))
	b.WriteString("\n")

	b.WriteString("Host\n")
	b.WriteString(fmt.Sprintf("  OS/Arch: %s/%s\n", r.Host.OS, r.Host.Arch))
	b.WriteString(fmt.Sprintf("  Host CPUs: %d (VM requested: %d)\n", r.Host.CPUCount, r.Host.RequestedCPU))
	b.WriteString("\n")

	b.WriteString("VM\n")
	b.WriteString(fmt.Sprintf("  Name: %s\n", r.VM.Name))
	b.WriteString(fmt.Sprintf("  Hostname: %s\n", r.VM.Hostname))
	b.WriteString(fmt.Sprintf("  Directory: %s\n", r.VM.Directory))
	b.WriteString(fmt.Sprintf("  Guest arch: %s\n", r.VM.GuestArch))
	b.WriteString(fmt.Sprintf("  CPU/Memory/Disk: %d vCPU / %d GiB / %d GiB\n", r.Host.RequestedCPU, r.VM.MemoryGiB, r.VM.DiskSizeGiB))
	b.WriteString(fmt.Sprintf("  Disk image: %s\n", r.VM.DiskPath))
	b.WriteString(fmt.Sprintf("  Cloud-init ISO: %s\n", r.VM.CloudInitISO))
	b.WriteString(fmt.Sprintf("  Console log: %s\n", r.VM.ConsoleLog))
	b.WriteString(fmt.Sprintf("  GUI console: %t\n", r.VM.GUIEnabled))
	b.WriteString("\n")

	b.WriteString("Network\n")
	b.WriteString(fmt.Sprintf("  Mode: %s\n", r.Network.Mode))
	if r.Network.BridgeInterface != "" {
		b.WriteString(fmt.Sprintf("  Bridge interface: %s\n", r.Network.BridgeInterface))
	}
	b.WriteString(fmt.Sprintf("  Guest MAC: %s\n", r.Network.MACAddress))
	b.WriteString(fmt.Sprintf("  Local SSH endpoint: %s\n", r.Network.LocalSSHEndpoint))
	b.WriteString(fmt.Sprintf("  Local API endpoint: %s\n", r.Network.LocalAPIEndpoint))
	b.WriteString(fmt.Sprintf("  Web dashboard: %s\n", r.Network.DashboardURL))
	b.WriteString("\n")

	b.WriteString("Incus\n")
	if r.Incus.ServerVersion != "" {
		b.WriteString(fmt.Sprintf("  Version/API: %s / %s\n", r.Incus.ServerVersion, r.Incus.APIVersion))
		b.WriteString(fmt.Sprintf("  Auth: %s\n", r.Incus.Auth))
		if len(r.Incus.Addresses) > 0 {
			b.WriteString(fmt.Sprintf("  Advertised addresses: %s\n", strings.Join(r.Incus.Addresses, ", ")))
		}
	} else {
		b.WriteString("  Status: API not ready yet; forwarding is active\n")
	}
	b.WriteString("\n")

	b.WriteString("Access\n")
	b.WriteString(fmt.Sprintf("  SSH: %s\n", r.Access.SSHCommand))
	if r.Access.SSHConfigPath != "" {
		b.WriteString(fmt.Sprintf("  SSH config: %s\n", r.Access.SSHConfigPath))
	}
	if r.Access.SSHKeyPath != "" {
		b.WriteString(fmt.Sprintf("  SSH key: %s\n", r.Access.SSHKeyPath))
	}
	b.WriteString(fmt.Sprintf("  REST: %s\n", r.Access.RESTExample))
	b.WriteString(fmt.Sprintf("  Go client example: %s\n", r.Access.GoClientExamplePath))
	b.WriteString(fmt.Sprintf("  Client cert/key: %s / %s\n", r.Access.ClientCertPath, r.Access.ClientKeyPath))
	b.WriteString(fmt.Sprintf("  Log file: %s\n", r.Access.LogPath))

	return b.String()
}
