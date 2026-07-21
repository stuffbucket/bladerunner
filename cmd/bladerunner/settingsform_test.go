//go:build darwin

package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stuffbucket/bladerunner/internal/config"
)

// A form generated from settings and posted back unchanged must round-trip to
// the same settings.
func TestSettingsFormRoundTrip(t *testing.T) {
	want := config.DefaultSettings()
	want.StartPolicy = config.StartOnLaunch
	want.CPUs = 6
	want.MemoryGiB = 12
	want.DiskSizeGiB = 80
	want.NetworkMode = config.NetSettingBridged
	want.BridgeInterface = "en3"
	want.NestedVirt = config.NestedDisabled
	want.WaitForIncus = config.Duration(7 * time.Minute)
	want.Image = config.ImageSource{Kind: config.ImageCustomURL, URL: "https://x/y.qcow2"}

	posted := valuesFromSettings(want)
	got, err := parseSettingsForm(posted, config.DefaultSettings())
	if err != nil {
		t.Fatalf("parseSettingsForm: %v", err)
	}
	if got != want {
		t.Errorf("round trip mismatch:\n got = %+v\nwant = %+v", got, want)
	}
}

func TestParseSettingsFormImageUnion(t *testing.T) {
	base := config.DefaultSettings()
	tests := []struct {
		name   string
		posted map[string]string
		want   config.ImageSource
	}{
		{"debian clears extras", map[string]string{fImageKind: "debian", fImageURL: "stale", fImagePath: "stale"}, config.ImageSource{Kind: config.ImageDebian}},
		{"custom url keeps only url", map[string]string{fImageKind: "custom-url", fImageURL: "https://x/y.qcow2", fImagePath: "stale"}, config.ImageSource{Kind: config.ImageCustomURL, URL: "https://x/y.qcow2"}},
		{"local path keeps only path", map[string]string{fImageKind: "local-path", fImageURL: "stale", fImagePath: "/tmp/i.raw"}, config.ImageSource{Kind: config.ImageLocalPath, Path: "/tmp/i.raw"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSettingsForm(tt.posted, base)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got.Image != tt.want {
				t.Errorf("Image = %+v, want %+v", got.Image, tt.want)
			}
		})
	}
}

func TestParseSettingsFormValidationErrors(t *testing.T) {
	base := config.DefaultSettings()
	tests := []struct {
		name   string
		posted map[string]string
	}{
		{"cpus non-numeric", map[string]string{fCPUs: "lots"}},
		{"cpus zero fails validate", map[string]string{fCPUs: "0"}},
		{"memory too low", map[string]string{fMemoryGiB: "1"}},
		{"disk too small", map[string]string{fDiskSizeGiB: "4"}},
		{"bad duration", map[string]string{fWaitForIncus: "soon"}},
		{"bad start policy", map[string]string{fStartPolicy: "whenever"}},
		{"custom url without url", map[string]string{fImageKind: "custom-url", fImageURL: ""}},
		{"bridged without iface", map[string]string{fNetworkMode: "bridged", fBridgeIface: ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseSettingsForm(tt.posted, base); err == nil {
				t.Errorf("expected error for %s", tt.name)
			}
		})
	}
}

// A partial post must keep base values for untouched fields.
func TestParseSettingsFormPartialKeepsBase(t *testing.T) {
	base := config.DefaultSettings()
	base.CPUs = 8
	base.MemoryGiB = 16
	got, err := parseSettingsForm(map[string]string{fCPUs: "2"}, base)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.CPUs != 2 {
		t.Errorf("CPUs = %d, want 2", got.CPUs)
	}
	if got.MemoryGiB != 16 {
		t.Errorf("MemoryGiB = %d, want base 16", got.MemoryGiB)
	}
}

func TestSettingsRequiresRestart(t *testing.T) {
	base := config.DefaultSettings()

	// StartPolicy is menubar-only: no restart.
	p := base
	p.StartPolicy = config.StartOnLaunch
	if settingsRequiresRestart(base, p) {
		t.Error("StartPolicy change should not require a restart")
	}

	// No change: no restart.
	if settingsRequiresRestart(base, base) {
		t.Error("identical settings should not require a restart")
	}

	for _, mut := range []func(*config.Settings){
		func(s *config.Settings) { s.CPUs = 99 },
		func(s *config.Settings) { s.MemoryGiB = 99 },
		func(s *config.Settings) { s.DiskSizeGiB = 99 },
		func(s *config.Settings) { s.NetworkMode = config.NetSettingBridged; s.BridgeInterface = "en9" },
		func(s *config.Settings) { s.NestedVirt = config.NestedDisabled },
		func(s *config.Settings) { s.Image = config.ImageSource{Kind: config.ImageDebian} },
	} {
		n := base
		mut(&n)
		if !settingsRequiresRestart(base, n) {
			t.Errorf("mutation %+v should require a restart", n)
		}
	}
}

func TestApplySettingsForm(t *testing.T) {
	dir := t.TempDir()

	// Valid save with no restart-relevant change while stopped -> Close.
	postValid := map[string]string{
		fStartPolicy: string(config.StartOnLaunch),
		fCPUs:        "4", fMemoryGiB: "8", fDiskSizeGiB: "64",
		fNetworkMode: "shared", fNestedVirt: "auto",
		fImageKind: "debian", fWaitForIncus: "10m",
	}
	raw := mustJSON(t, postValid)
	out := applySettingsForm(raw, dir, false)
	if out.IsError || !out.Close {
		t.Fatalf("valid save: got %+v, want Close", out)
	}
	// It persisted: reload and check StartPolicy stuck.
	got, err := config.LoadSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.StartPolicy != config.StartOnLaunch {
		t.Errorf("persisted StartPolicy = %q, want on-launch", got.StartPolicy)
	}

	// Invalid form -> error, no close.
	bad := map[string]string{fCPUs: "0"}
	out = applySettingsForm(mustJSON(t, bad), dir, false)
	if !out.IsError || out.Close {
		t.Errorf("invalid save: got %+v, want IsError", out)
	}

	// Malformed JSON -> error.
	out = applySettingsForm("{not json", dir, false)
	if !out.IsError {
		t.Errorf("malformed JSON: got %+v, want IsError", out)
	}

	// Restart-relevant change while running -> notice (not Close).
	postCPU := map[string]string{fCPUs: "8"}
	out = applySettingsForm(mustJSON(t, postCPU), dir, true)
	if out.IsError || out.Close || out.Message == "" {
		t.Errorf("restart-needed save: got %+v, want a non-error notice", out)
	}
}

func mustJSON(t *testing.T, m map[string]string) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSettingsFormHTMLReflectsSettings(t *testing.T) {
	s := config.DefaultSettings()
	s.CPUs = 6
	s.NetworkMode = config.NetSettingBridged
	s.Image = config.ImageSource{Kind: config.ImageCustomURL, URL: "https://example.test/i.qcow2"}
	htmlOut := settingsFormHTML(s)

	for _, want := range []string{
		`name="cpus"`,
		`value="6"`,
		`value="bridged" selected`,
		`value="custom-url" selected`,
		"https://example.test/i.qcow2",
		`messageHandlers.bladerunner.postMessage`,
	} {
		if !strings.Contains(htmlOut, want) {
			t.Errorf("generated HTML missing %q", want)
		}
	}
}
