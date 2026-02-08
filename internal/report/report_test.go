package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testReport() *StartupReport {
	return &StartupReport{
		GeneratedAt: time.Date(2026, 2, 8, 12, 0, 0, 0, time.UTC),
		Host: HostInfo{
			OS:           "darwin",
			Arch:         "arm64",
			CPUCount:     10,
			RequestedCPU: 4,
		},
		VM: VMInfo{
			Name:         "bladerunner",
			Hostname:     "bladerunner",
			Directory:    "/tmp/bladerunner",
			DiskPath:     "/tmp/bladerunner/disk.raw",
			DiskSizeGiB:  64,
			MemoryGiB:    8,
			GuestArch:    "aarch64",
			GUIEnabled:   false,
			ConsoleLog:   "/tmp/bladerunner/console.log",
			CloudInitISO: "/tmp/bladerunner/cloud-init.iso",
			BaseImageURL: "https://example.com/image.img",
		},
		Network: NetInfo{
			Mode:             "shared",
			MACAddress:       "02:00:00:12:34:56",
			LocalSSHEndpoint: "127.0.0.1:6022",
			LocalAPIEndpoint: "https://127.0.0.1:18443",
			DashboardURL:     "https://127.0.0.1:18443/ui",
		},
		Incus: IncusInfo{
			ServerVersion: "5.0.0",
			APIVersion:    "1.0",
			Auth:          "tls",
			ServerName:    "bladerunner",
			Addresses:     []string{"10.0.0.1", "fd00::1"},
			APIExtensions: 42,
		},
		Access: Access{
			SSHCommand:          "ssh -F /tmp/config bladerunner",
			SSHConfigPath:       "/tmp/ssh/config",
			SSHKeyPath:          "/tmp/ssh/id_ed25519",
			RESTExample:         "curl -k https://127.0.0.1:18443/1.0",
			GoClientExamplePath: "/tmp/bladerunner/incus-client-example.go",
			ClientCertPath:      "/tmp/bladerunner/client.crt",
			ClientKeyPath:       "/tmp/bladerunner/client.key",
			LogPath:             "/tmp/bladerunner/bladerunner.log",
		},
	}
}

func TestSaveJSON(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "report.json")

	report := testReport()
	if err := SaveJSON(path, report); err != nil {
		t.Fatalf("SaveJSON() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	var loaded StartupReport
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if loaded.VM.Name != "bladerunner" {
		t.Errorf("VM.Name = %q, want %q", loaded.VM.Name, "bladerunner")
	}
	if loaded.Host.Arch != "arm64" {
		t.Errorf("Host.Arch = %q, want %q", loaded.Host.Arch, "arm64")
	}
	if loaded.Incus.ServerVersion != "5.0.0" {
		t.Errorf("Incus.ServerVersion = %q, want %q", loaded.Incus.ServerVersion, "5.0.0")
	}
}

func TestSaveJSON_InvalidPath(t *testing.T) {
	report := testReport()
	err := SaveJSON("/nonexistent/dir/report.json", report)
	if err == nil {
		t.Error("SaveJSON() should fail for invalid path")
	}
}

func TestRenderText(t *testing.T) {
	report := testReport()
	text := RenderText(report)

	expectedStrings := []string{
		"Bladerunner Startup Report",
		"Generated: 2026-02-08T12:00:00Z",
		"Host",
		"OS/Arch: darwin/arm64",
		"Host CPUs: 10 (VM requested: 4)",
		"VM",
		"Name: bladerunner",
		"Network",
		"Mode: shared",
		"Guest MAC: 02:00:00:12:34:56",
		"Local SSH endpoint: 127.0.0.1:6022",
		"Incus",
		"Version/API: 5.0.0 / 1.0",
		"Auth: tls",
		"Advertised addresses: 10.0.0.1, fd00::1",
		"Access",
		"SSH: ssh -F /tmp/config bladerunner",
		"REST: curl -k https://127.0.0.1:18443/1.0",
	}

	for _, expected := range expectedStrings {
		if !strings.Contains(text, expected) {
			t.Errorf("RenderText() missing %q", expected)
		}
	}
}

func TestRenderText_IncusNotReady(t *testing.T) {
	report := testReport()
	report.Incus = IncusInfo{}

	text := RenderText(report)

	if !strings.Contains(text, "API not ready yet") {
		t.Error("RenderText() should show 'API not ready' when Incus has no version")
	}
	if strings.Contains(text, "Version/API:") {
		t.Error("RenderText() should not show Version/API when Incus not ready")
	}
}

func TestRenderText_BridgeMode(t *testing.T) {
	report := testReport()
	report.Network.Mode = "bridged"
	report.Network.BridgeInterface = "en0"

	text := RenderText(report)

	if !strings.Contains(text, "Mode: bridged") {
		t.Error("RenderText() missing bridge mode")
	}
	if !strings.Contains(text, "Bridge interface: en0") {
		t.Error("RenderText() missing bridge interface")
	}
}

func TestRenderText_NoOptionalFields(t *testing.T) {
	report := testReport()
	report.Access.SSHConfigPath = ""
	report.Access.SSHKeyPath = ""

	text := RenderText(report)

	if strings.Contains(text, "SSH config:") {
		t.Error("RenderText() should omit SSH config when empty")
	}
	if strings.Contains(text, "SSH key:") {
		t.Error("RenderText() should omit SSH key when empty")
	}
}
