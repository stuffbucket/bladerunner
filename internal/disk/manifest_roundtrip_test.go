// Round-trip cover for disk.Manifest, the ".disk" file a user edits by hand and
// `br disk new` writes. Every exported field of the manifest and of its nested
// specs (ImageSpec, ArchImage, VMSpec, BootSpec, ShareSpec) carries a json tag,
// so each one is an on-disk format that a later build — or the `br disk ls` of a
// different process — reads back. The export behavior is the write and the
// import behavior is the read.
//
// These tests live in the external test package so they see Manifest exactly as
// cmd/bladerunner and internal/vmhost do. An unexported field, a lost tag or a
// duplicated tag drops the value in silence on the way through the file, and
// only a full-struct comparison of a fully populated fixture catches that.
//
// The write side is util.WriteJSONAtomic and the read side is disk.Load /
// disk.Parse, which is the pair production uses: `br disk new` publishes the
// manifest with WriteJSONAtomic, the catalog reads user disks with Load and
// embedded builtins with Parse.
package disk_test

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/disk"
	"github.com/stuffbucket/bladerunner/internal/util"
)

const (
	// manifestPerm mirrors the mode cmd/bladerunner publishes a .disk with: a
	// manifest is meant to be readable and editable by its owner.
	manifestPerm fs.FileMode = 0o644

	// Sizing values are deliberately none of the config defaults, so a field
	// that fails to survive the trip cannot come back looking correct.
	fixtureCPUs        = 6
	fixtureMemoryGiB   = 12
	fixtureDiskSizeGiB = 48

	// Two distinct 64-character lowercase hex digests. Validate() rejects any
	// other shape, and distinct values keep a swapped arch entry visible.
	fixtureARM64SHA256 = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"
	fixtureAMD64SHA256 = "9a8b7c6d5e4f30219a8b7c6d5e4f30219a8b7c6d5e4f30219a8b7c6d5e4f3021"

	archARM64 = "arm64"
	archAMD64 = "amd64"
)

// roundTripFixture returns a Manifest with every exported field — its own and
// every nested spec's — set to a distinct, non-zero value, apart from the image
// source that variant does not use.
//
// Distinct matters: two string fields holding the same value would hide a
// swapped json tag. Non-zero matters because a field that fails to survive the
// trip comes back as the zero value, which a fixture full of zeros cannot tell
// from a value that arrived.
//
// ImageSpec cannot be filled in one shot: Validate() rejects a manifest that
// sets more than one of image.arches, image.path and image.hosted, so the three
// sources are covered by three variants instead. Together they cross every
// field of ImageSpec.
func roundTripFixture(source imageSource) disk.Manifest {
	return disk.Manifest{
		Name:        "roundtrip-disk",
		Description: "round-trip fixture disk (not a real image)",
		Version:     "2026.01.02",
		Image:       source.spec(),
		VM: disk.VMSpec{
			CPUs:        fixtureCPUs,
			MemoryGiB:   fixtureMemoryGiB,
			DiskSizeGiB: fixtureDiskSizeGiB,
		},
		Boot: disk.BootSpec{
			Mode:      disk.BootModeGUI,
			Autologin: true,
		},
		Share: &disk.ShareSpec{
			Tag:       "roundtrip-share",
			ReadOnly:  true,
			GuestPath: "/mnt/roundtrip",
		},
	}
}

// imageSource names one of the three mutually exclusive ways a manifest points
// at a qcow2.
type imageSource string

const (
	sourceArches imageSource = "arches"
	sourcePath   imageSource = "path"
	sourceHosted imageSource = "hosted"
)

// spec returns the ImageSpec for this source, with every field that the source
// uses set non-zero.
func (s imageSource) spec() disk.ImageSpec {
	switch s {
	case sourceArches:
		return disk.ImageSpec{Arches: map[string]disk.ArchImage{
			archARM64: {URL: "https://example.invalid/roundtrip-arm64.qcow2", SHA256: fixtureARM64SHA256},
			archAMD64: {URL: "https://example.invalid/roundtrip-amd64.qcow2", SHA256: fixtureAMD64SHA256},
		}}
	case sourcePath:
		return disk.ImageSpec{Path: "/var/tmp/roundtrip.qcow2"}
	case sourceHosted:
		return disk.ImageSpec{Hosted: true}
	default:
		panic("unknown image source " + string(s))
	}
}

// allSources is the set of variants every round-trip test runs over.
var allSources = []imageSource{sourceArches, sourcePath, sourceHosted}

// TestManifestRoundTripFixtureCoversEveryField fails when a field is added to
// Manifest or to one of its nested specs without being added to the fixture.
// Without it a new field would round-trip zero-to-zero and every test below
// would pass while proving nothing about that field.
//
// A field counts as covered when ANY variant sets it, because the three image
// sources exclude one another by design.
func TestManifestRoundTripFixtureCoversEveryField(t *testing.T) {
	zeroEverywhere := zeroFieldPaths("Manifest", reflect.ValueOf(roundTripFixture(allSources[0])))
	for _, source := range allSources[1:] {
		zeroHere := make(map[string]bool)
		for _, p := range zeroFieldPaths("Manifest", reflect.ValueOf(roundTripFixture(source))) {
			zeroHere[p] = true
		}
		kept := zeroEverywhere[:0]
		for _, p := range zeroEverywhere {
			if zeroHere[p] {
				kept = append(kept, p)
			}
		}
		zeroEverywhere = kept
	}
	for _, p := range zeroEverywhere {
		t.Errorf("%s is zero in every round-trip fixture: give it a distinct non-zero value", p)
	}
}

// zeroFieldPaths returns the dotted paths of the exported leaves under v that
// hold their zero value. A nil pointer and an empty map are themselves zero
// leaves: neither carries anything across the file.
func zeroFieldPaths(prefix string, v reflect.Value) []string {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return []string{prefix}
		}
		return zeroFieldPaths(prefix, v.Elem())
	case reflect.Map:
		if v.Len() == 0 {
			return []string{prefix}
		}
		out := make([]string, 0, v.Len())
		for _, k := range v.MapKeys() {
			out = append(out, zeroFieldPaths(fmt.Sprintf("%s[%v]", prefix, k), v.MapIndex(k))...)
		}
		return out
	case reflect.Struct:
		typ := v.Type()
		out := make([]string, 0, typ.NumField())
		for i := range typ.NumField() {
			field := typ.Field(i)
			if !field.IsExported() {
				continue
			}
			out = append(out, zeroFieldPaths(prefix+"."+field.Name, v.Field(i))...)
		}
		return out
	default:
		if v.IsZero() {
			return []string{prefix}
		}
		return nil
	}
}

// TestManifestRoundTripThroughWriteLoad exercises the path production uses for
// a user disk: `br disk new` publishes the manifest with util.WriteJSONAtomic,
// and the catalog reads it back later with disk.Load, from another process and
// possibly another build.
func TestManifestRoundTripThroughWriteLoad(t *testing.T) {
	for _, source := range allSources {
		t.Run(string(source), func(t *testing.T) {
			want := roundTripFixture(source)
			path := filepath.Join(t.TempDir(), want.Name+disk.ManifestExt)

			if err := util.WriteJSONAtomic(path, &want, manifestPerm); err != nil {
				t.Fatalf("WriteJSONAtomic: %v", err)
			}
			got, err := disk.Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if !reflect.DeepEqual(*got, want) {
				t.Errorf("manifest did not survive write/load:\n got = %+v\nwant = %+v", *got, want)
			}
		})
	}
}

// TestManifestRoundTripThroughParse holds the same claim for the bytes path:
// the catalog decodes each embedded builtin with disk.Parse, never touching a
// file. A manifest must therefore be recoverable from its own JSON alone.
func TestManifestRoundTripThroughParse(t *testing.T) {
	for _, source := range allSources {
		t.Run(string(source), func(t *testing.T) {
			want := roundTripFixture(source)

			raw, err := json.Marshal(&want)
			if err != nil {
				t.Fatalf("marshal manifest: %v", err)
			}
			got, err := disk.Parse(raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if !reflect.DeepEqual(*got, want) {
				t.Errorf("manifest did not survive JSON:\n got = %+v\nwant = %+v\nraw = %s", *got, want, raw)
			}
		})
	}
}

// TestManifestRoundTripKeepsOneKeyPerField proves each populated field reaches
// the file under its own key, at every level. Two fields that share a tag are
// dropped by encoding/json without any complaint, and both would then read back
// as zero; a person editing a .disk by hand reads these keys, not the Go struct.
//
// The claim is "one key per field the fixture set", not "one key per field":
// image.path and image.hosted are `omitempty` and mutually exclusive with
// image.arches, so a correct arches manifest legitimately omits them. The check
// therefore reads the written value alongside the struct value and demands a
// key for each non-zero field and no key that no field produced.
func TestManifestRoundTripKeepsOneKeyPerField(t *testing.T) {
	for _, source := range allSources {
		t.Run(string(source), func(t *testing.T) {
			m := roundTripFixture(source)
			path := filepath.Join(t.TempDir(), m.Name+disk.ManifestExt)
			if err := util.WriteJSONAtomic(path, &m, manifestPerm); err != nil {
				t.Fatalf("WriteJSONAtomic: %v", err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}

			var doc map[string]any
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("written manifest is not valid JSON: %v\nraw = %s", err, raw)
			}
			checkKeys(t, "manifest", reflect.ValueOf(m), doc)
		})
	}
}

// checkKeys asserts that doc holds one key per exported field of the struct
// value v that JSON writes, under the name the tag gives, and descends into
// every nested object. A field that is zero and tagged `omitempty` must be
// absent; every other field must be present.
func checkKeys(t *testing.T, path string, v reflect.Value, doc map[string]any) {
	t.Helper()

	written := 0
	typ := v.Type()
	for i := range typ.NumField() {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		name, omitEmpty := jsonKey(field)
		if name == "" {
			continue // explicitly not serialized (`json:"-"`)
		}
		value, present := doc[name]
		if omitEmpty && v.Field(i).IsZero() {
			if present {
				t.Errorf("%s.%s: key %q written for a zero omitempty field", path, field.Name, name)
			}
			continue
		}
		written++
		if !present {
			t.Errorf("%s.%s: no key %q in the written manifest", path, field.Name, name)
			continue
		}
		descend(t, path+"."+name, v.Field(i), value)
	}
	if len(doc) != written {
		t.Errorf("%s has %d keys for %d written fields: %v", path, len(doc), written, doc)
	}
}

// descend follows one written value into the nested struct or per-arch map it
// came from, so ImageSpec, VMSpec, BootSpec, ShareSpec and ArchImage are all
// checked, not the top level alone.
func descend(t *testing.T, path string, v reflect.Value, value any) {
	t.Helper()

	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Struct:
		nested, ok := value.(map[string]any)
		if !ok {
			t.Errorf("%s: expected a JSON object for %s, got %T", path, v.Type(), value)
			return
		}
		checkKeys(t, path, v, nested)
	case reflect.Map:
		entries, ok := value.(map[string]any)
		if !ok {
			t.Errorf("%s: expected a JSON object for %s, got %T", path, v.Type(), value)
			return
		}
		if len(entries) != v.Len() {
			t.Errorf("%s has %d entries for %d map keys: %v", path, len(entries), v.Len(), entries)
		}
		for key, entry := range entries {
			descend(t, fmt.Sprintf("%s[%s]", path, key), v.MapIndex(reflect.ValueOf(key)), entry)
		}
	default:
		// A scalar leaf: the struct comparisons above already hold its value.
	}
}

// jsonKey returns the key a field is written under, and whether the tag carries
// `omitempty`. The key is "" when the field is never serialized.
func jsonKey(field reflect.StructField) (name string, omitEmpty bool) {
	tag, ok := field.Tag.Lookup("json")
	if !ok {
		return field.Name, false
	}
	name, opts, _ := strings.Cut(tag, ",")
	omitEmpty = slices.Contains(strings.Split(opts, ","), "omitempty")
	switch name {
	case "-":
		return "", omitEmpty
	case "":
		return field.Name, omitEmpty
	default:
		return name, omitEmpty
	}
}

// TestManifestRoundTripBuiltinManifests runs the real shipped .disk files
// through the same trip. The fixture proves the schema can carry a value; these
// prove the manifests bladerunner actually ships survive being read, rewritten
// by `br disk new --from`, and read again — which is what a fork of a builtin
// does.
func TestManifestRoundTripBuiltinManifests(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("manifests", "*"+disk.ManifestExt))
	if err != nil {
		t.Fatalf("glob builtin manifests: %v", err)
	}
	if len(paths) == 0 {
		t.Fatalf("no builtin manifests found: the catalog embeds manifests/*%s", disk.ManifestExt)
	}

	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			want, err := disk.Load(path)
			if err != nil {
				t.Fatalf("Load %s: %v", path, err)
			}

			rewritten := filepath.Join(t.TempDir(), filepath.Base(path))
			if err := util.WriteJSONAtomic(rewritten, want, manifestPerm); err != nil {
				t.Fatalf("WriteJSONAtomic: %v", err)
			}
			got, err := disk.Load(rewritten)
			if err != nil {
				t.Fatalf("Load rewritten %s: %v", path, err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s did not survive a rewrite:\n got = %+v\nwant = %+v", path, *got, *want)
			}
		})
	}
}
