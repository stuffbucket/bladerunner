// These round-trip tests live in the external test package so they exercise
// Settings exactly as another package (or another version of bladerunner) sees
// it: only exported names, only the json tags. AGENTS.md rule 5.5 asks for this
// shape — an exported struct field is held by a write-then-read-then-compare of
// the type, because for a json-tagged field the export behavior is the write
// and the import behavior is the read.
package config_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/stuffbucket/bladerunner/internal/config"
)

// fullSettings returns a Settings with every exported field set to a distinct
// non-zero value, so a field that fails to survive a round trip (missing tag,
// unexported, wrong type) shows up as a difference rather than as two zero
// values that happen to agree.
//
// SchemaVersion is the one field that cannot carry an arbitrary value through
// the file path: Save stamps the current schema version over whatever the
// caller held. It takes the DefaultSettings value here, which is that same
// non-zero version, so the file round trip and the JSON round trip can share
// one fixture. TestSettingsJSONRoundTripSchemaVersion holds the tag itself.
//
// The image union is a parameter because only one of its two string fields may
// be populated at a time (ImageSource.Valid rejects both), so covering URL and
// Path needs two trips through the file.
func fullSettings(image config.ImageSource) config.Settings {
	return config.Settings{
		SchemaVersion: config.DefaultSettings().SchemaVersion,
		StartPolicy:   config.StartOnFirstAction,
		CPUs:          6,
		MemoryGiB:     12,
		DiskSizeGiB:   config.MinDiskSizeGiB + 13,
		// Bridged rather than shared so BridgeInterface is both non-zero and
		// legal: Validate requires the interface name for this mode, and the
		// field is omitempty, so an empty one would never reach the file.
		NetworkMode:     config.NetSettingBridged,
		BridgeInterface: "en7",
		Image:           image,
		NestedVirt:      config.NestedDisabled,
		WaitForIncus:    config.Duration(7*time.Minute + 30*time.Second),
		ShowConsole:     true,
	}
}

// assertNoZeroFields fails if any exported field of s is still at its zero
// value. It is the guard that keeps this file honest as Settings grows: a field
// added later without a line in fullSettings would otherwise be "round-tripped"
// as zero-to-zero and prove nothing.
func assertNoZeroFields(t *testing.T, s config.Settings) {
	t.Helper()
	v := reflect.ValueOf(s)
	for i := range v.NumField() {
		if v.Field(i).IsZero() {
			t.Errorf("fixture leaves %s at its zero value; give it a distinct value",
				v.Type().Field(i).Name)
		}
	}
}

// TestSettingsSaveLoadRoundTripAllFields writes a fully populated Settings with
// the helpers production uses (Save, then LoadSettings) and compares the whole
// struct that comes back. Save/LoadSettings are the pair under test rather than
// json.Marshal because they are what the settings screen and start-time
// reconciliation call: they add the atomic write, the defaults overlay and the
// validation that a raw Marshal skips.
func TestSettingsSaveLoadRoundTripAllFields(t *testing.T) {
	images := map[string]config.ImageSource{
		// Two trips, because ImageSource is a union: URL and Path are each
		// omitempty and each illegal beside the other, so one trip can only
		// carry one of them.
		"custom url": {Kind: config.ImageCustomURL, URL: "https://example.test/base.qcow2"},
		"local path": {Kind: config.ImageLocalPath, Path: "/tmp/base.raw"},
	}
	for name, image := range images {
		t.Run(name, func(t *testing.T) {
			want := fullSettings(image)
			assertNoZeroFields(t, want)
			if err := want.Validate(); err != nil {
				t.Fatalf("fixture must be a valid Settings, got: %v", err)
			}

			dir := t.TempDir()
			if err := want.Save(dir); err != nil {
				t.Fatalf("Save: %v", err)
			}
			got, err := config.LoadSettings(dir)
			if err != nil {
				t.Fatalf("LoadSettings: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Settings did not survive the file round trip:\n got = %+v\nwant = %+v", got, want)
			}
		})
	}
}

// TestSettingsJSONRoundTripAllFields holds the encoding on its own, without the
// defaults overlay LoadSettings applies. The overlay starts from
// DefaultSettings, so a field whose tag went missing could still come back
// looking right there if the fixture happened to match the default; decoding
// into a zero Settings removes that cover.
func TestSettingsJSONRoundTripAllFields(t *testing.T) {
	want := fullSettings(config.ImageSource{Kind: config.ImageCustomURL, URL: "https://example.test/base.qcow2"})
	assertNoZeroFields(t, want)

	b, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got config.Settings
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Settings did not survive the JSON round trip:\n got = %+v\nwant = %+v\njson = %s", got, want, b)
	}
}

// TestSettingsJSONRoundTripSchemaVersion holds the one field the file round
// trip cannot: Save overwrites SchemaVersion with the current version, so the
// file path can never show that the tag carries an arbitrary value. A reader of
// a document written by a later version must still see the version it holds,
// otherwise a format change cannot be detected.
func TestSettingsJSONRoundTripSchemaVersion(t *testing.T) {
	const futureVersion = 7
	want := fullSettings(config.ImageSource{Kind: config.ImageHosted})
	want.SchemaVersion = futureVersion

	b, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got config.Settings
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.SchemaVersion != futureVersion {
		t.Errorf("SchemaVersion = %d, want %d (json = %s)", got.SchemaVersion, futureVersion, b)
	}
}

// TestImageSourceJSONRoundTripAllFields covers ImageSource on its own with all
// three fields populated at once. The union rules keep URL and Path apart in a
// valid Settings, and both are omitempty, so only a deliberately invalid value
// can prove that each one carries a tag of its own. Encoding does not validate,
// so this is safe here and nowhere else.
func TestImageSourceJSONRoundTripAllFields(t *testing.T) {
	want := config.ImageSource{
		Kind: config.ImageLocalPath,
		URL:  "https://example.test/base.qcow2",
		Path: "/tmp/base.raw",
	}
	v := reflect.ValueOf(want)
	for i := range v.NumField() {
		if v.Field(i).IsZero() {
			t.Errorf("fixture leaves %s at its zero value", v.Type().Field(i).Name)
		}
	}

	b, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got config.ImageSource
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ImageSource did not survive the JSON round trip:\n got = %+v\nwant = %+v\njson = %s", got, want, b)
	}
}
