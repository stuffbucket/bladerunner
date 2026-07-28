package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

// Bounds for the concurrent-reader hammer below.
const (
	// reportPadBytes makes the payload big enough that a truncating write has a
	// window a concurrent reader can land in.
	reportPadBytes = 1 << 18
	// reportRewrites is how many times the writer republishes the report.
	reportRewrites = 200
	// reportReaders is how many goroutines read the report concurrently.
	reportReaders = 4
)

// A reader must never see the startup report missing, empty or short while it
// is being rewritten: os.WriteFile opens O_TRUNC, so the destination is briefly
// zero-length. Only a temp-file-and-rename publish makes the swap atomic.
func TestSaveJSONNeverExposesAPartialFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "startup-report.json")
	r := testReport()
	r.Access.RESTExample = strings.Repeat("x", reportPadBytes)
	if err := SaveJSON(path, r); err != nil {
		t.Fatalf("seed SaveJSON: %v", err)
	}
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read seeded report: %v", err)
	}
	wantLen := len(full)

	var partials atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range reportReaders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				b, readErr := os.ReadFile(path)
				if readErr != nil || len(b) != wantLen {
					partials.Add(1)
				}
			}
		}()
	}
	for range reportRewrites {
		if err := SaveJSON(path, r); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("SaveJSON: %v", err)
		}
	}
	close(stop)
	wg.Wait()

	if n := partials.Load(); n != 0 {
		t.Errorf("a concurrent reader saw a partial startup report %d times (want %d bytes every time): the publish is not atomic", n, wantLen)
	}
}

// The report keeps its published mode across a rewrite. os.CreateTemp makes
// 0600 files, so an atomic publish that forgets to chmod would narrow it.
func TestSaveJSONKeepsFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "startup-report.json")
	for i := range 2 {
		if err := SaveJSON(path, testReport()); err != nil {
			t.Fatalf("SaveJSON #%d: %v", i, err)
		}
		st, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat report #%d: %v", i, err)
		}
		if perm := st.Mode().Perm(); perm != reportFilePerm {
			t.Errorf("report mode after write #%d = %o, want %o", i, perm, reportFilePerm)
		}
	}
}

// A completed publish must leave no staging file next to the report.
func TestSaveJSONLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "startup-report.json")
	if err := SaveJSON(path, testReport()); err != nil {
		t.Fatalf("SaveJSON: %v", err)
	}
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(des) != 1 || des[0].Name() != "startup-report.json" {
		t.Errorf("report directory has %d entries, want only the report", len(des))
	}
}
