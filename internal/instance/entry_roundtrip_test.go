package instance_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/stuffbucket/bladerunner/internal/instance"
)

// roundTripPID is the PID recorded in the sample entry. It is only ever
// written and read back — nothing in this file probes liveness — but it must
// be non-zero so that a field that silently fails to survive the trip shows up
// as a difference rather than as a zero that happened to match.
const roundTripPID = 4194305

// fullEntry returns an Entry with every exported field set to a distinct
// non-zero value.
//
// Distinct matters as much as non-zero. Two fields of the same type holding the
// same value hide a swap: an encoder that wrote SourcePath into WorkingCopy
// would still compare equal. Every string below therefore differs from every
// other, and every int differs from every other.
//
// entryIsFullyPopulated below walks the result with reflect and fails if any
// exported field is still zero, so a field added to Entry later cannot slip
// past this test by being forgotten here.
func fullEntry() instance.Entry {
	return instance.Entry{
		Name:              "round-trip",
		Kind:              instance.KindCartridge,
		StateDir:          "/Volumes/round-trip",
		SourcePath:        "/Users/someone/source.dmg",
		WorkingCopy:       "/Volumes/round-trip/working.img",
		DevNode:           "/dev/disk7",
		Mountpoint:        "/Volumes/round-trip-mount",
		UnmountProtection: instance.ProtectionArmed,
		PID:               roundTripPID,
		Ports:             instance.Ports{SSH: 6022, API: 18443, Web: 18444, OIDC: 15556, NTP: 10123},
		ProtocolVersion:   3,
		BinaryVersion:     "v1.2.3",
		StartedAt:         time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC),
		GUI:               true,
	}
}

// entryIsFullyPopulated fails t if any exported field of v — recursing into
// nested structs such as Ports — is still its zero value. It is the guard that
// keeps this test honest as Entry grows: a round-trip test that leaves a new
// field at zero proves nothing about that field, because zero survives every
// broken encoder.
func entryIsFullyPopulated(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	typ := v.Type()
	for i := range typ.NumField() {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		name := path + field.Name
		value := v.Field(i)
		if value.Kind() == reflect.Struct && field.Type != reflect.TypeOf(time.Time{}) {
			entryIsFullyPopulated(t, value, name+".")
			continue
		}
		if value.IsZero() {
			t.Errorf("%s is zero: populate it, or the round trip proves nothing about it", name)
		}
	}
}

// TestEntryRoundTripThroughRegistry is the round-trip test AGENTS.md rule 5.5
// requires for the exported fields of Entry, exercised through the pair
// production actually uses: instance.Write publishes the record and
// instance.Read decodes it back.
//
// It runs from the external test package on purpose (rule 5.4): a holder
// process and the CLI are different packages from this one, and a field that is
// unexported, or missing a json tag, only fails from outside.
//
// ProtocolVersion is called out in AGENTS.md section 9, point 3 as a field that
// looks dead to a static analyser — nothing in this build reads it, because the
// version that reads it has not been written yet. Its whole purpose is to
// survive the trip to disk and back, so this test is the only thing standing
// between it and a well-meaning deletion.
func TestEntryRoundTripThroughRegistry(t *testing.T) {
	dir := t.TempDir()
	want := fullEntry()
	entryIsFullyPopulated(t, reflect.ValueOf(want), "Entry.")

	if err := instance.Write(dir, want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := instance.Read(dir, want.Name)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	// StartedAt is compared with Equal, not with the struct: the value that
	// comes back off disk carries no monotonic reading and its own *Location,
	// neither of which a byte-wise comparison forgives.
	if !got.StartedAt.Equal(want.StartedAt) {
		t.Errorf("StartedAt = %v, want %v", got.StartedAt, want.StartedAt)
	}
	got.StartedAt = want.StartedAt

	if !reflect.DeepEqual(got, want) {
		t.Errorf("entry did not survive Write/Read:\n got %+v\nwant %+v", got, want)
	}
}

// TestEntryRoundTripThroughJSON holds the same claim one level lower, against
// encoding/json alone.
//
// The registry is not the only reader of this format. The record is a file on
// disk that a different bladerunner version, or an operator with jq, decodes
// without going through instance.Read — and instance.Read defaults Name from
// the file name, which would mask a Name field that lost its tag. Decoding the
// bytes directly removes that safety net.
func TestEntryRoundTripThroughJSON(t *testing.T) {
	want := fullEntry()
	entryIsFullyPopulated(t, reflect.ValueOf(want), "Entry.")

	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got instance.Entry
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if !got.StartedAt.Equal(want.StartedAt) {
		t.Errorf("StartedAt = %v, want %v", got.StartedAt, want.StartedAt)
	}
	got.StartedAt = want.StartedAt

	if !reflect.DeepEqual(got, want) {
		t.Errorf("entry did not survive Marshal/Unmarshal:\n got %+v\nwant %+v\njson %s", got, want, data)
	}
}
