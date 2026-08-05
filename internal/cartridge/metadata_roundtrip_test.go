// Round-trip cover for cartridge.Metadata, the on-image self-description at
// MetadataFile. Every field is a json-tagged on-disk format that a different
// build — or a different process — reads back, so the export behavior is the
// write and the import behavior is the read.
//
// These tests live in the external test package so they see Metadata exactly as
// another package does: an unexported or untagged field would drop its value
// silently on the way through the file, and only a full-struct comparison
// catches that.
package cartridge_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/cartridge"
)

// roundTripFixture is a Metadata with every exported field set to a distinct,
// non-zero value.
//
// Distinct matters: two fields of the same type holding the same value would
// hide a swapped json tag. Non-zero matters twice over — a field that fails to
// survive the trip comes back as the zero value, and WriteMetadata fills a zero
// FormatVersion and a zero PackedAt in for the caller, which would mask a lost
// value as a defaulted one.
func roundTripFixture() cartridge.Metadata {
	return cartridge.Metadata{
		FormatVersion: cartridge.FormatVersion,
		Name:          "roundtrip-cartridge",
		PackedBy:      "br 0.0.0-roundtrip",
		PackedAt:      "2026-01-02T03:04:05Z",
	}
}

// TestMetadataRoundTripFixtureCoversEveryField fails when a field is added to
// Metadata without being added to the fixture. Without it, a new field would
// round-trip as zero-to-zero and the tests below would pass while proving
// nothing about it.
func TestMetadataRoundTripFixtureCoversEveryField(t *testing.T) {
	v := reflect.ValueOf(roundTripFixture())
	typ := v.Type()
	for i := range typ.NumField() {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		if v.Field(i).IsZero() {
			t.Errorf("Metadata.%s is zero in the round-trip fixture: give it a distinct non-zero value", field.Name)
		}
	}
}

// TestMetadataRoundTripThroughWriteRead exercises the path production uses:
// WriteMetadata stamps the file into a mounted cartridge and ReadMetadata reads
// it back on the next open, possibly from a different build.
func TestMetadataRoundTripThroughWriteRead(t *testing.T) {
	dir := t.TempDir()
	want := roundTripFixture()

	if err := cartridge.WriteMetadata(dir, want); err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}
	got, err := cartridge.ReadMetadata(dir)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("metadata did not survive write/read:\n got = %+v\nwant = %+v", got, want)
	}
}

// TestMetadataRoundTripThroughJSON holds the same claim one layer down, at the
// encoding itself. The file on a cartridge is read by whatever opens the
// volume, so the struct must be recoverable from its own JSON alone, with no
// help from WriteMetadata's defaulting.
func TestMetadataRoundTripThroughJSON(t *testing.T) {
	want := roundTripFixture()

	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	var got cartridge.Metadata
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("metadata did not survive JSON:\n got = %+v\nwant = %+v\nraw = %s", got, want, raw)
	}
}

// TestMetadataRoundTripKeepsOneKeyPerField proves each field reaches the file
// under its own key. A field that shares a key with another, or that carries no
// tag at all, would still pass a struct comparison in the pair above if the
// values happened to line up; this reads the written file as a plain map, which
// is also how a person or a future tool inspects a cartridge by hand.
func TestMetadataRoundTripKeepsOneKeyPerField(t *testing.T) {
	dir := t.TempDir()
	if err := cartridge.WriteMetadata(dir, roundTripFixture()); err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, cartridge.MetadataFile))
	if err != nil {
		t.Fatalf("read %s: %v", cartridge.MetadataFile, err)
	}

	var onDisk map[string]any
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("cartridge metadata is not valid JSON: %v", err)
	}
	exported := 0
	typ := reflect.TypeOf(cartridge.Metadata{})
	for i := range typ.NumField() {
		if typ.Field(i).IsExported() {
			exported++
		}
	}
	if len(onDisk) != exported {
		t.Errorf("metadata file has %d keys for %d exported fields: %s", len(onDisk), exported, raw)
	}
}
