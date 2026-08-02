package report_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/stuffbucket/bladerunner/internal/report"
)

// fullStartupReport builds a StartupReport in which every exported field of
// every section holds a distinct non-zero value. Distinct matters as much as
// non-zero: two fields that share a value hide a swap between them, and a field
// left at its zero value hides a lost `json:` tag, because a dropped field
// decodes back to that same zero value and compares equal.
//
// Add a field to any section here as soon as you add it to report.go. The
// assertEveryFieldSet guard below fails the round-trip test until you do.
func fullStartupReport() report.StartupReport {
	return report.StartupReport{
		// A UTC instant with a non-zero nanosecond part. RFC 3339 is what
		// encoding/json writes for a time.Time, and it carries sub-second
		// digits, so a truncating format change shows up here.
		GeneratedAt: time.Date(2026, time.March, 14, 15, 9, 26, 535897932, time.UTC),
		Host: report.HostInfo{
			OS:           "darwin",
			Arch:         "arm64",
			CPUCount:     12,
			RequestedCPU: 6,
		},
		VM: report.VMInfo{
			Name:          "roundtrip-vm",
			Hostname:      "roundtrip-host",
			Directory:     "/var/roundtrip/vm",
			DiskPath:      "/var/roundtrip/vm/disk.raw",
			DiskSizeGiB:   128,
			MemoryGiB:     24,
			GuestArch:     "aarch64",
			GUIEnabled:    true, // false is the zero value and proves nothing
			ConsoleLog:    "/var/roundtrip/vm/console.log",
			CloudInitISO:  "/var/roundtrip/vm/cloud-init.iso",
			BaseImageURL:  "https://example.invalid/base.img",
			BaseImagePath: "/var/roundtrip/images/base.img",
		},
		Network: report.NetInfo{
			Mode:             "bridged",
			BridgeInterface:  "en7", // only bridged mode fills this one
			MACAddress:       "02:00:00:ab:cd:ef",
			LocalSSHEndpoint: "127.0.0.1:6122",
			LocalAPIEndpoint: "https://127.0.0.1:18543",
			DashboardURL:     "https://127.0.0.1:18543/ui",
		},
		Incus: report.IncusInfo{
			ServerVersion: "6.1.0",
			APIVersion:    "1.0",
			Auth:          "trusted",
			ServerName:    "roundtrip-incus",
			Addresses:     []string{"10.1.2.3:8443", "[fd00::2]:8443"},
			APIExtensions: 314,
		},
		Access: report.Access{
			SSHCommand:          "ssh -F /var/roundtrip/ssh/config roundtrip-vm",
			SSHConfigPath:       "/var/roundtrip/ssh/config",
			SSHKeyPath:          "/var/roundtrip/ssh/id_ed25519",
			RESTExample:         "curl -k https://127.0.0.1:18543/1.0",
			GoClientExamplePath: "/var/roundtrip/vm/incus-client-example.go",
			ClientCertPath:      "/var/roundtrip/vm/client.crt",
			ClientKeyPath:       "/var/roundtrip/vm/client.key",
			LogPath:             "/var/roundtrip/vm/bladerunner.log",
		},
	}
}

// assertEveryFieldSet fails when any exported field reachable from v is still
// at its zero value. It descends into nested structs, so HostInfo, VMInfo,
// NetInfo, IncusInfo and Access are covered field by field, and it treats
// time.Time as a leaf because its own fields are unexported.
//
// This is what keeps the round-trip honest over time. Without it, a field added
// to report.go later is silently absent from the fixture, and the comparison
// below then passes whether or not that field survives the write and the read.
func assertEveryFieldSet(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	if v.IsZero() {
		t.Errorf("%s is at its zero value: the fixture must give every exported field a distinct non-zero value, or the round trip cannot see the field being dropped", path)
		return
	}
	if v.Kind() != reflect.Struct || v.Type() == reflect.TypeOf(time.Time{}) {
		return
	}
	for i := range v.NumField() {
		field := v.Type().Field(i)
		if !field.IsExported() {
			continue
		}
		assertEveryFieldSet(t, v.Field(i), path+"."+field.Name)
	}
}

// TestStartupReportRoundTrip holds AGENTS.md section 5.5 for report.StartupReport
// and for the HostInfo, VMInfo, NetInfo, IncusInfo and Access values nested in
// it. Every one of those fields carries a `json:` tag, which makes the struct an
// on-disk format: SaveJSON is the export, and the operator's tooling reading
// startup-report.json back is the import. The test does both halves and compares
// the whole struct, so a field that loses its tag, gains a mismatched name, or
// stops being exported fails here rather than in someone else's parser.
//
// The two subtests are the two shapes production hands out: the file that
// SaveJSON publishes, and the JSON encoding itself, which is what any other
// version of the struct decodes.
func TestStartupReportRoundTrip(t *testing.T) {
	want := fullStartupReport()
	assertEveryFieldSet(t, reflect.ValueOf(want), "StartupReport")

	t.Run("through SaveJSON and the published file", func(t *testing.T) {
		// SaveJSON is the writer production uses; it does not create the parent
		// directory, so the report goes straight into the temp dir.
		path := filepath.Join(t.TempDir(), "startup-report.json")
		if err := report.SaveJSON(path, &want); err != nil {
			t.Fatalf("SaveJSON: %v", err)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read published report: %v", err)
		}

		var got report.StartupReport
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("decode published report: %v", err)
		}

		if !reflect.DeepEqual(got, want) {
			t.Errorf("the report did not survive SaveJSON and a read back\n got: %+v\nwant: %+v\n file: %s", got, want, data)
		}
	})

	t.Run("through the JSON encoding another version decodes", func(t *testing.T) {
		data, err := json.Marshal(&want)
		if err != nil {
			t.Fatalf("encode report: %v", err)
		}

		var got report.StartupReport
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("decode report: %v", err)
		}

		if !reflect.DeepEqual(got, want) {
			t.Errorf("the report did not survive a JSON encode and decode\n got: %+v\nwant: %+v\n wire: %s", got, want, data)
		}
	})
}
