package vmhost_test

// A Spec crosses a PROCESS boundary: cmd/bladerunner/vmgate.go marshals it to
// JSON and writes the blob into the state directory, and the holder — a
// separate `br vmd` process — reads that file back and runs the instance from
// it. Every exported field on Spec and Overrides is therefore an on-disk
// format. A field that silently fails the trip (a missing tag, a type JSON
// cannot carry) does not fail the build and does not fail a unit test of
// Validate; it arrives at the holder as a zero value and the instance boots
// wrong.
//
// The tests here are in the EXTERNAL test package on purpose: they see Spec
// exactly as cmd/bladerunner sees it, so a field that is only reachable from
// inside vmhost cannot make a round trip look successful.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/stuffbucket/bladerunner/internal/cartridge"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/disk"
	"github.com/stuffbucket/bladerunner/internal/instance"
	"github.com/stuffbucket/bladerunner/internal/util"
	"github.com/stuffbucket/bladerunner/internal/vmhost"
)

// specHandoffPerm mirrors the mode cmd/bladerunner writes the hand-off file
// with, so the file hop under test is the one production performs.
const specHandoffPerm = 0o600

// specJSONIgnoredField is the one exported field of Spec that must NOT survive
// the trip. Config is tagged `json:"-"` because it is the in-process escape
// hatch for a caller that already resolved a config: the paths in it mean
// nothing to another process, and it may hold live handles. See the field's
// own doc comment.
const specJSONIgnoredField = "Config"

// fullSpec returns a Spec with every exported field set to a distinct non-zero
// value, except Config (see specJSONIgnoredField).
//
// Distinct values matter: two fields that hold the same value cannot tell a
// swapped pair apart, and a zero value cannot tell "carried" apart from
// "dropped". TestSpecRoundTripCoversEveryField holds this property, so a field
// added to Spec later fails that test until it is added here too.
func fullSpec() vmhost.Spec {
	return vmhost.Spec{
		Name:          "roundtrip",
		Kind:          instance.KindCartridge,
		StateDir:      "/state/roundtrip",
		CartridgePath: "/cartridges/roundtrip.dmg",
		Mountpoint:    "/state/roundtrip/mnt/roundtrip",
		MountPolicy:   cartridge.MountPrivate,
		Persist:       true,
		Manifest:      fullManifest(),
		Overrides:     fullOverrides(),
		ChangedFlags:  []string{"cpus", "memory", "disk", "gui", "timeout"},
		Driven:        true,
		RestoreFrom:   "/state/roundtrip/saved.vzvmsave",
		Ports: config.PortAssignment{
			SSH:  20122,
			API:  20123,
			Web:  20124,
			OIDC: 20125,
			NTP:  20126,
		},
		DrainTimeout:  91 * time.Second,
		BinaryVersion: "v9.9.9-roundtrip",
	}
}

// fullOverrides returns Overrides with every exported field set to a distinct
// non-zero value. The booleans can only be true, so each one is carried by its
// own name; the scalars are all different from each other.
func fullOverrides() vmhost.Overrides {
	return vmhost.Overrides{
		CPUs:         7,
		MemoryGiB:    11,
		DiskSizeGiB:  23,
		GUI:          true,
		ImageURL:     "https://example.invalid/roundtrip.qcow2",
		ImagePath:    "/images/roundtrip.qcow2",
		HostedImage:  true,
		DebianImage:  true,
		NoNestedVirt: true,
		Timeout:      13 * time.Minute,
	}
}

// fullManifest returns a disk manifest with every nested field populated. The
// manifest rides inside the Spec, so its own fields make the same crossing.
func fullManifest() *disk.Manifest {
	return &disk.Manifest{
		Name:        "roundtrip",
		Description: "a manifest that must survive the hand-off",
		Version:     "2026.07.29",
		Image: disk.ImageSpec{
			Arches: map[string]disk.ArchImage{
				"arm64": {
					URL:    "https://example.invalid/arm64.qcow2",
					SHA256: "1111111111111111111111111111111111111111111111111111111111111111",
				},
				"amd64": {
					URL:    "https://example.invalid/amd64.qcow2",
					SHA256: "2222222222222222222222222222222222222222222222222222222222222222",
				},
			},
		},
		VM:   disk.VMSpec{CPUs: 5, MemoryGiB: 9, DiskSizeGiB: 64},
		Boot: disk.BootSpec{Mode: disk.BootModeGUI, Autologin: true},
		Share: &disk.ShareSpec{
			Tag:       "roundtrip-share",
			ReadOnly:  true,
			GuestPath: "/mnt/roundtrip",
		},
	}
}

// TestSpecRoundTripCoversEveryField holds the fixture honest. It is the reason
// the round-trip tests below are worth anything: a field added to Spec or
// Overrides tomorrow arrives here as a zero value, this test names it, and the
// round trip then has to prove it crosses.
func TestSpecRoundTripCoversEveryField(t *testing.T) {
	assertEveryFieldSet(t, "Spec", fullSpec(), specJSONIgnoredField)
	assertEveryFieldSet(t, "Overrides", fullOverrides())
}

// assertEveryFieldSet reports each exported field of v that holds its zero
// value, other than the named exemptions.
func assertEveryFieldSet(t *testing.T, what string, v any, exempt ...string) {
	t.Helper()
	rv := reflect.ValueOf(v)
	for i := range rv.NumField() {
		f := rv.Type().Field(i)
		if !f.IsExported() || slices.Contains(exempt, f.Name) {
			continue
		}
		if rv.Field(i).IsZero() {
			t.Errorf("%s.%s is zero in the fixture: a dropped field would look identical to a carried one", what, f.Name)
		}
	}
}

// TestSpecRoundTripJSON is the hop itself, with no file in the way: marshal the
// Spec the way vmgate.go does and unmarshal it the way vmd.go does, then
// compare the WHOLE struct. A full comparison is what catches the field nobody
// thought to check.
func TestSpecRoundTripJSON(t *testing.T) {
	want := fullSpec()

	blob, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}

	var got vmhost.Spec
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("unmarshal spec: %v", err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("spec did not survive the JSON round trip\n got: %+v\nwant: %+v\nJSON: %s", got, want, blob)
	}
}

// TestSpecRoundTripHandoffFile performs the crossing the way the two processes
// actually perform it: an atomic write of the JSON blob into the state
// directory (internal/util owns atomic writes), then a read from a caller that
// only has the path.
func TestSpecRoundTripHandoffFile(t *testing.T) {
	want := fullSpec()

	blob, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	path := filepath.Join(t.TempDir(), "roundtrip.json")
	if err := util.WriteFileAtomic(path, blob, specHandoffPerm); err != nil {
		t.Fatalf("write hand-off file: %v", err)
	}

	// From here on, pretend to be the holder: the path is all it was given.
	read, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hand-off file: %v", err)
	}
	var got vmhost.Spec
	if err := json.Unmarshal(read, &got); err != nil {
		t.Fatalf("decode hand-off file: %v", err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("spec did not survive the hand-off file\n got: %+v\nwant: %+v", got, want)
	}
}

// TestSpecRoundTripStillValidates holds the trip end to end at the level the
// holder cares about: a Spec that Validate accepted before the crossing must
// still be accepted after it. A field that arrived zero can fail Validate on
// the far side, which is the failure mode the holder reports as a launch error
// with no hint that the write was the cause.
func TestSpecRoundTripStillValidates(t *testing.T) {
	t.Setenv(config.ForceHostedImageEnvVar, "")
	t.Setenv(config.ForceDebianImageEnvVar, "")

	// fullSpec sets every image override at once, which Validate rejects as a
	// conflict by design; drop the conflicting ones for this check, because
	// what is under test here is the crossing, not the flag conflict.
	want := fullSpec()
	want.Overrides.HostedImage = false
	want.Overrides.DebianImage = false
	want.Overrides.ImageURL = ""
	want.Overrides.ImagePath = ""
	// A cartridge always cold-boots, so it cannot carry a restore file.
	want.RestoreFrom = ""
	if err := want.Validate(); err != nil {
		t.Fatalf("the fixture must be valid before the trip: %v", err)
	}

	blob, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	var got vmhost.Spec
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("unmarshal spec: %v", err)
	}

	if err := got.Validate(); err != nil {
		t.Errorf("spec became invalid by crossing the process boundary: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("spec did not survive the JSON round trip\n got: %+v\nwant: %+v", got, want)
	}
}

// TestSpecRoundTripDropsConfig holds the one field that must NOT cross. Config
// is the in-process escape hatch: it may carry live handles and paths that only
// the process that built it can read, so the hand-off must leave it behind
// rather than hand the holder something it would treat as resolved.
func TestSpecRoundTripDropsConfig(t *testing.T) {
	spec := fullSpec()
	cfg, err := config.Default(spec.StateDir)
	if err != nil {
		t.Fatalf("resolve a base config: %v", err)
	}
	spec.Config = cfg

	blob, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	var got vmhost.Spec
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("unmarshal spec: %v", err)
	}

	if got.Config != nil {
		t.Errorf("Config crossed the process boundary: %+v", got.Config)
	}
	// Everything else must still be there: dropping Config must not drop its
	// neighbors.
	spec.Config = nil
	if !reflect.DeepEqual(got, spec) {
		t.Errorf("spec did not survive the JSON round trip\n got: %+v\nwant: %+v", got, spec)
	}
}
