// This file lives in the external test package so it exercises SaveMetadata
// exactly as a different package — or a different version of the program —
// sees it: only the exported fields and the exported sidecar helpers. The
// in-package tests in savestate_test.go can reach writeSaveMetadata; a reader
// of the on-disk sidecar cannot, so the read path must stand on its own.
package vm_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/vm"
)

// TestSaveMetadataRoundTripAllFields writes a fully-populated SaveMetadata to
// the sidecar path and reads it back with LoadSaveMetadata. Every exported
// field carries a distinct non-zero value, so a field that does not survive the
// trip — a missing json tag, a tag that does not agree with the reader, a field
// that became unexported — shows up as a difference in the compared struct
// instead of passing in silence. The sidecar is an on-disk format that a later
// version reads (AGENTS.md section 9, point 3), so the write and the read are
// the export and import behavior of these fields.
func TestSaveMetadataRoundTripAllFields(t *testing.T) {
	gui := true
	want := vm.SaveMetadata{
		CPUs:              6,
		MemoryGiB:         12,
		DiskSizeGiB:       128,
		DiskPath:          "/var/lib/bladerunner/disk.raw",
		GUI:               &gui,
		ShareTag:          "bladerunner-roundtrip-share",
		DiskSizeBytes:     137438953472,
		DiskMtimeUnixNano: 1718000000123456789,
		DiskInode:         987654321,
	}

	// A zero value in any exported field would make that field's round trip
	// unobservable — an encoder that dropped it and a decoder that never saw it
	// give the same result. Check the fixture before it is used, so a field
	// added later without a value here fails loudly rather than going untested.
	assertNoZeroExportedField(t, want)

	savePath := filepath.Join(t.TempDir(), "saved-state.bin")

	// The write. Production writes the sidecar with json.MarshalIndent through
	// the unexported writeSaveMetadata; from outside the package the marshal is
	// all that is reachable, and it is the same encoder over the same tags.
	b, err := json.MarshalIndent(&want, "", "  ")
	if err != nil {
		t.Fatalf("marshal SaveMetadata: %v", err)
	}
	if err := os.WriteFile(vm.SaveMetadataPath(savePath), b, 0o600); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	// The read. This is the exported entry point the restore path uses.
	got, err := vm.LoadSaveMetadata(savePath)
	if err != nil {
		t.Fatalf("LoadSaveMetadata: %v", err)
	}

	// Compare the whole struct, not a selection of fields. A cherry-picked
	// comparison passes over the one field that was lost.
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("SaveMetadata did not survive the round trip\n got: %+v\nwant: %+v", *got, want)
	}
	// DeepEqual on a *bool compares the pointed-to value, so state the pointer
	// case as well: a decoder that produced a nil GUI would be a skipped restore
	// parity check, which is the failure this field exists to prevent.
	if got.GUI == nil {
		t.Error("GUI decoded to nil; the restore GUI-parity check would be skipped")
	} else if *got.GUI != *want.GUI {
		t.Errorf("GUI = %v, want %v", *got.GUI, *want.GUI)
	}
}

// TestSaveMetadataRoundTripThroughSavedSidecar re-encodes what LoadSaveMetadata
// returned and reads it once more. A field that decodes but does not re-encode
// — or a tag the writer and the reader spell differently — survives one pass
// and is lost on the second, which is what happens in service when one version
// reads a sidecar and writes it back for the next.
func TestSaveMetadataRoundTripThroughSavedSidecar(t *testing.T) {
	gui := false
	first := vm.SaveMetadata{
		CPUs:              2,
		MemoryGiB:         4,
		DiskSizeGiB:       32,
		DiskPath:          "/var/lib/bladerunner/second.raw",
		GUI:               &gui, // non-nil and false: recorded, not absent
		ShareTag:          "second-pass-share",
		DiskSizeBytes:     34359738368,
		DiskMtimeUnixNano: 1718000999987654321,
		DiskInode:         123456789,
	}
	assertNoZeroExportedField(t, first)

	dir := t.TempDir()
	firstPath := filepath.Join(dir, "first.bin")
	secondPath := filepath.Join(dir, "second.bin")

	b, err := json.Marshal(&first)
	if err != nil {
		t.Fatalf("marshal first: %v", err)
	}
	if err := os.WriteFile(vm.SaveMetadataPath(firstPath), b, 0o600); err != nil {
		t.Fatalf("write first sidecar: %v", err)
	}
	mid, err := vm.LoadSaveMetadata(firstPath)
	if err != nil {
		t.Fatalf("LoadSaveMetadata first: %v", err)
	}

	b2, err := json.Marshal(mid)
	if err != nil {
		t.Fatalf("marshal second: %v", err)
	}
	if err := os.WriteFile(vm.SaveMetadataPath(secondPath), b2, 0o600); err != nil {
		t.Fatalf("write second sidecar: %v", err)
	}
	got, err := vm.LoadSaveMetadata(secondPath)
	if err != nil {
		t.Fatalf("LoadSaveMetadata second: %v", err)
	}

	if !reflect.DeepEqual(*got, first) {
		t.Errorf("SaveMetadata did not survive two round trips\n got: %+v\nwant: %+v", *got, first)
	}
}

// assertNoZeroExportedField fails when any exported field of m holds its zero
// value. It keeps the round-trip fixtures honest as fields are added: a new
// field left unset in the fixture is a field the round trip does not test.
func assertNoZeroExportedField(t *testing.T, m vm.SaveMetadata) {
	t.Helper()
	v := reflect.ValueOf(m)
	typ := v.Type()
	for i := range typ.NumField() {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		if v.Field(i).IsZero() {
			t.Fatalf("field %s is the zero value; give it a distinct non-zero value so the round trip covers it", f.Name)
		}
	}
}
