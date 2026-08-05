// This file is the external view of the package: it imports bootstage the way
// the menubar does, so it holds the export behavior (the write) and the import
// behavior (the read) of every exported field of State together.
package bootstage_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/stuffbucket/bladerunner/internal/bootstage"
)

// roundTripFixture is a State in which every exported field carries a distinct
// non-zero value. Distinct values matter: two fields that shared a value could
// swap places on disk and the comparison would still pass. Non-zero matters
// because a field that never survives the trip comes back as its zero value,
// which is exactly what this test must catch.
//
// The timestamp keeps sub-second precision and a fixed UTC location so the
// comparison also holds the JSON time encoding to nanoseconds, and so the test
// does not depend on the clock or the host time zone.
func roundTripFixture() bootstage.State {
	return bootstage.State{
		Stage:     bootstage.Ejecting,
		UpdatedAt: time.Date(2026, time.July, 29, 13, 14, 15, 123456789, time.UTC),
		Phase:     bootstage.PhaseShutdown,
		Detail:    bootstage.DetailForced,
	}
}

// TestStateRoundTripAllFields writes a fully populated State with the helpers
// production uses and reads it back in one piece. boot-stage.json is read by the
// menubar in a different process, so a field that loses its json tag, becomes
// unexported or changes type breaks a cross-process contract that no in-process
// test would notice.
func TestStateRoundTripAllFields(t *testing.T) {
	dir := t.TempDir()
	want := roundTripFixture()

	if err := bootstage.WriteState(dir, want); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	got, ok := bootstage.Read(dir)
	if !ok {
		t.Fatal("Read: ok=false after WriteState")
	}

	// Compare the whole struct, not a chosen few fields: a cherry-picked
	// comparison goes on passing when a new field is added and does not
	// survive the trip.
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip changed the state:\n got %#v\nwant %#v", got, want)
	}
}

// TestStateRoundTripFixtureIsComplete guards the fixture itself. It fails when
// someone adds an exported field to State and does not give it a value above,
// which would otherwise leave the new field untested in silence.
func TestStateRoundTripFixtureIsComplete(t *testing.T) {
	v := reflect.ValueOf(roundTripFixture())
	typ := v.Type()
	for i := range typ.NumField() {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		if v.Field(i).IsZero() {
			t.Errorf("field %s is zero in the round-trip fixture; give it a distinct non-zero value", f.Name)
		}
	}
}

// TestWriteRoundTripDerivesPhase covers the other producer entry point. Write
// takes only a stage, and WriteState fills Phase in from it, so what lands on
// disk — and therefore what the menubar reads — carries the phase even though
// the caller never named one.
func TestWriteRoundTripDerivesPhase(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, time.July, 29, 8, 9, 10, 0, time.UTC)

	if err := bootstage.Write(dir, bootstage.Incus, now); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, ok := bootstage.Read(dir)
	if !ok {
		t.Fatal("Read: ok=false after Write")
	}

	want := bootstage.State{
		Stage:     bootstage.Incus,
		UpdatedAt: now,
		Phase:     bootstage.PhaseBoot,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip changed the state:\n got %#v\nwant %#v", got, want)
	}
}
