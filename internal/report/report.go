package report

import (
	"encoding/json"
	"fmt"
	"os"
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
