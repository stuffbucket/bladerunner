package boot

import (
	"strings"
	"testing"
)

func TestParseReader_KernelBoot(t *testing.T) {
	input := "[    0.000000] Linux version 6.8.0-47-generic"
	status := ParseReader(strings.NewReader(input))

	if !status.KernelBooted {
		t.Error("expected KernelBooted to be true")
	}
}

func TestParseReader_CloudInitComplete(t *testing.T) {
	input := "Cloud-init v. 24.1 finished at 2024-01-01 00:00:00"
	status := ParseReader(strings.NewReader(input))

	if !status.CloudInitDone {
		t.Error("expected CloudInitDone to be true")
	}
}

func TestParseReader_CloudInitFailed(t *testing.T) {
	input := "cloud-init error: DataSource not found"
	status := ParseReader(strings.NewReader(input))

	if !status.CloudInitFailed {
		t.Error("expected CloudInitFailed to be true")
	}
	if len(status.Errors) == 0 {
		t.Error("expected error to be captured")
	}
}

func TestParseReader_KernelPanic(t *testing.T) {
	input := "Kernel panic - not syncing: VFS: Unable to mount root fs"
	status := ParseReader(strings.NewReader(input))

	if !status.KernelPanic {
		t.Error("expected KernelPanic to be true")
	}
	if status.Healthy() {
		t.Error("expected Healthy() to return false after kernel panic")
	}
}

func TestParseReader_EmergencyMode(t *testing.T) {
	input := "You are in emergency mode. After logging in"
	status := ParseReader(strings.NewReader(input))

	if !status.EmergencyMode {
		t.Error("expected EmergencyMode to be true")
	}
}

func TestParseReader_SSHReady(t *testing.T) {
	input := "Started SSH server"
	status := ParseReader(strings.NewReader(input))

	if !status.SSHReady {
		t.Error("expected SSHReady to be true")
	}
}

func TestParseReader_IncusReady(t *testing.T) {
	input := "incusd daemon started"
	status := ParseReader(strings.NewReader(input))

	if !status.IncusReady {
		t.Error("expected IncusReady to be true")
	}
}

func TestStatus_Summary(t *testing.T) {
	tests := []struct {
		name   string
		status Status
		want   string
	}{
		{"panic", Status{KernelPanic: true}, "kernel panic detected"},
		{"emergency", Status{EmergencyMode: true}, "systemd emergency mode"},
		{"cloud-init fail", Status{CloudInitFailed: true}, "cloud-init failed"},
		{"not booted", Status{}, "kernel not booted"},
		{"no systemd", Status{KernelBooted: true}, "waiting for systemd"},
		{"complete", Status{KernelBooted: true, SystemdReached: true, CloudInitDone: true, SSHReady: true, IncusReady: true}, "boot complete"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.status.Summary(); got != tt.want {
				t.Errorf("Summary() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStatus_Healthy(t *testing.T) {
	tests := []struct {
		name   string
		status Status
		want   bool
	}{
		{"panic", Status{CloudInitDone: true, KernelPanic: true}, false},
		{"emergency", Status{CloudInitDone: true, EmergencyMode: true}, false},
		{"cloud-init failed", Status{CloudInitDone: true, CloudInitFailed: true}, false},
		{"incomplete", Status{}, false},
		{"healthy", Status{CloudInitDone: true}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.status.Healthy(); got != tt.want {
				t.Errorf("Healthy() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsNoiseError(t *testing.T) {
	tests := []struct {
		line  string
		noise bool
	}{
		{"error=0", true},
		{"No error reported", true},
		{"real error: disk full", false},
		{"failed to start service", false},
	}

	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			if got := isNoiseError(tt.line); got != tt.noise {
				t.Errorf("isNoiseError(%q) = %v, want %v", tt.line, got, tt.noise)
			}
		})
	}
}

func TestExtractError_Truncation(t *testing.T) {
	long := strings.Repeat("x", 300)
	result := extractError(long)

	if len(result) > 210 {
		t.Errorf("extractError did not truncate: len=%d", len(result))
	}
	if !strings.HasSuffix(result, "...") {
		t.Error("expected truncated string to end with ...")
	}
}
