package update_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/update"
)

// manifestFilePerm is the mode of the latest.json fixture this test writes.
// The real file is published by the release workflow, not by us; the fixture
// only has to be readable by the same process.
const manifestFilePerm = 0o600

// roundTripManifest returns a Manifest with every exported field set to a
// distinct non-zero value. Distinct values matter: if two fields shared a
// value, a marshaller that crossed their JSON tags would still compare equal
// after the trip and the test would pass on a broken format.
func roundTripManifest() update.Manifest {
	return update.Manifest{
		Version:   "0.4.8",
		URL:       "https://github.com/stuffbucket/bladerunner/releases/download/v0.4.8/Bladerunner.app.tar.gz",
		Signature: "dW50cnVzdGVkIGNvbW1lbnQ6IHNpZ25hdHVyZQpSV1FmSHh5dVczRmx2eXo=",
		Notes:     "round trip release notes",
		PubDate:   "2026-07-20T00:00:00Z",
	}
}

// TestManifestFileRoundTrip writes a fully populated Manifest to disk as
// latest.json, reads it back, and compares the whole struct. This is the shape
// cmd/update-manifest writes into the Pages build output and that fetchManifest
// parses after the download, so the write is the export behavior and the read is
// the import behavior of every json-tagged field (AGENTS.md 5.5).
//
// The test lives in package update_test on purpose (AGENTS.md 5.4): it sees
// Manifest exactly as cmd/bladerunner and any future reader of the manifest
// sees it. A field that loses its tag, loses its export, or changes type is
// caught here rather than at the next release.
func TestManifestFileRoundTrip(t *testing.T) {
	want := roundTripManifest()
	requireAllFieldsSet(t, want)

	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}

	// A real reader gets the manifest as bytes on disk or on the wire, never as
	// a live Go value, so send it through a file before reading it back.
	path := filepath.Join(t.TempDir(), "latest.json")
	if err := os.WriteFile(path, raw, manifestFilePerm); err != nil {
		t.Fatalf("write latest.json: %v", err)
	}
	readBack, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read latest.json: %v", err)
	}

	var got update.Manifest
	if err := json.Unmarshal(readBack, &got); err != nil {
		t.Fatalf("unmarshal latest.json: %v", err)
	}
	// Compare the whole struct. A field-by-field comparison would go stale the
	// day somebody adds a field and forgets this test.
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

// TestManifestFetchRoundTrip sends the same manifest over the path production
// uses: an HTTPS GET parsed by update.Check. Check is the only exported entry
// that reads a Manifest, so it is the read helper this rule asks us to prefer
// over a bare json.Unmarshal. It surfaces Version and Notes, and it fails the
// fetch outright if URL or Signature did not survive, because validate()
// rejects a manifest that is missing either one.
func TestManifestFetchRoundTrip(t *testing.T) {
	want := roundTripManifest()
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	// An older running version keeps UpdateAvailable true, which proves Version
	// arrived in a form isNewer could parse and not just as a matching string.
	const oldVersion = "0.0.1"
	res, err := update.Check(context.Background(), update.Options{
		CurrentVersion: oldVersion,
		ManifestURL:    srv.URL,
		HTTPClient:     srv.Client(),
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.LatestVersion != want.Version {
		t.Errorf("LatestVersion = %q, want %q", res.LatestVersion, want.Version)
	}
	if res.Notes != want.Notes {
		t.Errorf("Notes = %q, want %q", res.Notes, want.Notes)
	}
	if !res.UpdateAvailable {
		t.Errorf("UpdateAvailable = false, want true for running %q vs manifest %q", oldVersion, want.Version)
	}
}

// requireAllFieldsSet fails when any exported field of the fixture is still at
// its zero value. Without it, a field added to Manifest later would ride
// through the round trip as "" == "" and the comparison above would pass while
// covering nothing.
func requireAllFieldsSet(t *testing.T, m update.Manifest) {
	t.Helper()
	v := reflect.ValueOf(m)
	typ := v.Type()
	for i := range typ.NumField() {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		if v.Field(i).IsZero() {
			t.Fatalf("fixture leaves exported field %s at its zero value; give it a distinct value", field.Name)
		}
	}
}
