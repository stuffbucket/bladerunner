package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/update"
)

// releaseListJSON renders releases the way `gh api .../releases` does.
func releaseListJSON(t *testing.T, releases []update.Release) string {
	t.Helper()
	raw, err := json.Marshal(releases)
	if err != nil {
		t.Fatalf("marshal releases: %v", err)
	}
	return string(raw)
}

// updaterRelease returns a product release carrying both updater assets.
func updaterRelease(tag string) update.Release {
	base := "https://github.com/stuffbucket/bladerunner/releases/download/" + tag + "/"
	return update.Release{
		TagName:     tag,
		Name:        "bladerunner " + tag,
		PublishedAt: "2026-08-05T00:00:00Z",
		Assets: []update.ReleaseAsset{
			{Name: update.UpdaterTarballName, DownloadURL: base + update.UpdaterTarballName},
			{Name: update.UpdaterSignatureName, DownloadURL: base + update.UpdaterSignatureName},
		},
	}
}

// writeSig writes a stand-in .sig file and returns its path.
func writeSig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), update.UpdaterSignatureName)
	const content = "untrusted comment: signature\nRWSfHxyuW3Flv1uFC87PAA==\ntrusted comment: t\nZm9v\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write signature file: %v", err)
	}
	return path
}

// TestRunSelect prints the tag of the release that can serve an update.
func TestRunSelect(t *testing.T) {
	in := strings.NewReader(releaseListJSON(t, []update.Release{
		updaterRelease("v0.4.8"),
		updaterRelease("v0.4.9"),
	}))
	var out bytes.Buffer
	if err := runSelect(in, &out, ""); err != nil {
		t.Fatalf("runSelect: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "v0.4.9" {
		t.Errorf("runSelect wrote %q, want %q", got, "v0.4.9")
	}
}

// TestRunSelect_NoUpdaterRelease is the graceful no-op the site build depends
// on: empty output and success, so the build publishes no manifest and carries
// on. Anything else would fail every site deploy until the first signed release.
func TestRunSelect_NoUpdaterRelease(t *testing.T) {
	in := strings.NewReader(releaseListJSON(t, []update.Release{
		{TagName: "guest-image-v2026.08.05"},
		{TagName: "v0.4.7"},
	}))
	var out bytes.Buffer
	if err := runSelect(in, &out, ""); err != nil {
		t.Fatalf("runSelect: %v", err)
	}
	if out.String() != "" {
		t.Errorf("runSelect wrote %q, want nothing", out.String())
	}
}

// TestRunSelect_BadInput fails rather than reporting an empty channel.
func TestRunSelect_BadInput(t *testing.T) {
	var out bytes.Buffer
	if err := runSelect(strings.NewReader("{not json"), &out, ""); err == nil {
		t.Fatal("runSelect accepted a malformed release list")
	}
}

// writeLive writes a published manifest fixture and returns its path.
func writeLive(t *testing.T, version string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "live.json")
	m := update.Manifest{
		Version:   version,
		URL:       "https://github.com/stuffbucket/bladerunner/releases/download/v" + version + "/" + update.UpdaterTarballName,
		Signature: "dW50cnVzdGVkIGNvbW1lbnQ6IHNpZ25hdHVyZQo=",
		Notes:     "published " + version,
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal live manifest: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write live manifest: %v", err)
	}
	return path
}

// TestRunEmit writes a manifest that the updater's own parser accepts.
func TestRunEmit(t *testing.T) {
	in := strings.NewReader(releaseListJSON(t, []update.Release{updaterRelease("v0.4.9")}))
	var out bytes.Buffer
	args := []string{"-signature", writeSig(t), "-live-manifest", writeLive(t, "0.4.8")}
	if err := runEmit(args, in, &out); err != nil {
		t.Fatalf("runEmit: %v", err)
	}
	var m update.Manifest
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("emitted manifest is not valid JSON: %v\n%s", err, out.String())
	}
	if m.Version != "0.4.9" {
		t.Errorf("version = %q, want %q", m.Version, "0.4.9")
	}
	if m.Signature == "" {
		t.Error("signature is empty")
	}
}

// TestRunEmit_RefusesDowngrade holds the rule salvaged from the abandoned
// publish workflow. Refusing means re-emitting what is already published, so
// the site deploy that called this cannot regress the update channel and cannot
// fail over a release that has nothing to do with it.
func TestRunEmit_RefusesDowngrade(t *testing.T) {
	in := strings.NewReader(releaseListJSON(t, []update.Release{updaterRelease("v0.4.7")}))
	var out bytes.Buffer
	args := []string{"-signature", writeSig(t), "-live-manifest", writeLive(t, "0.4.8")}
	if err := runEmit(args, in, &out); err != nil {
		t.Fatalf("runEmit: %v", err)
	}
	var m update.Manifest
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("emitted manifest is not valid JSON: %v", err)
	}
	if m.Version != "0.4.8" {
		t.Errorf("version = %q; the published 0.4.8 must survive the older 0.4.7", m.Version)
	}
}

// TestRunEmit_AllowDowngrade keeps a deliberate rollback possible.
func TestRunEmit_AllowDowngrade(t *testing.T) {
	in := strings.NewReader(releaseListJSON(t, []update.Release{updaterRelease("v0.4.7")}))
	var out bytes.Buffer
	args := []string{"-signature", writeSig(t), "-live-manifest", writeLive(t, "0.4.8"), "-allow-downgrade"}
	if err := runEmit(args, in, &out); err != nil {
		t.Fatalf("runEmit with -allow-downgrade: %v", err)
	}
	var m update.Manifest
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("emitted manifest is not valid JSON: %v", err)
	}
	if m.Version != "0.4.7" {
		t.Errorf("version = %q, want the rolled-back %q", m.Version, "0.4.7")
	}
}

// TestRunEmit_UnreadableLiveManifest publishes over a live manifest it cannot
// read. A corrupt or absent published file must not wedge the channel.
func TestRunEmit_UnreadableLiveManifest(t *testing.T) {
	corrupt := filepath.Join(t.TempDir(), "live.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt live manifest: %v", err)
	}
	for _, live := range []string{corrupt, filepath.Join(t.TempDir(), "missing.json"), ""} {
		in := strings.NewReader(releaseListJSON(t, []update.Release{updaterRelease("v0.4.9")}))
		var out bytes.Buffer
		args := []string{"-signature", writeSig(t)}
		if live != "" {
			args = append(args, "-live-manifest", live)
		}
		if err := runEmit(args, in, &out); err != nil {
			t.Fatalf("runEmit with live manifest %q: %v", live, err)
		}
		var m update.Manifest
		if err := json.Unmarshal(out.Bytes(), &m); err != nil {
			t.Fatalf("emitted manifest is not valid JSON: %v", err)
		}
		if m.Version != "0.4.9" {
			t.Errorf("version = %q with live manifest %q, want %q", m.Version, live, "0.4.9")
		}
	}
}

// TestRunEmit_NoUpdaterRelease fails: emit is only reached after select found a
// release, so an empty channel here means the release list changed underneath.
func TestRunEmit_NoUpdaterRelease(t *testing.T) {
	in := strings.NewReader(releaseListJSON(t, []update.Release{{TagName: "guest-image-v2026.08.05"}}))
	var out bytes.Buffer
	if err := runEmit([]string{"-signature", writeSig(t)}, in, &out); err == nil {
		t.Fatal("runEmit succeeded with no release that can serve an update")
	}
}

// TestRunEmit_RequiresSignature refuses to guess.
func TestRunEmit_RequiresSignature(t *testing.T) {
	in := strings.NewReader(releaseListJSON(t, []update.Release{updaterRelease("v0.4.9")}))
	var out bytes.Buffer
	if err := runEmit(nil, in, &out); err == nil {
		t.Fatal("runEmit ran without -signature")
	}
}

// TestRunSelect_TagFilter answers "is this one release ready to serve an
// update yet?", which is what the workflow that waits for the asynchronous
// signed assets polls on. An older release that is already publishable must not
// satisfy the wait for a newer tag.
func TestRunSelect_TagFilter(t *testing.T) {
	list := releaseListJSON(t, []update.Release{
		updaterRelease("v0.4.8"),
		{TagName: "v0.4.9", Name: "bladerunner v0.4.9"}, // tagged, assets not uploaded yet
	})

	var pending bytes.Buffer
	if err := runSelect(strings.NewReader(list), &pending, "v0.4.9"); err != nil {
		t.Fatalf("runSelect: %v", err)
	}
	if pending.String() != "" {
		t.Errorf("runSelect -tag v0.4.9 wrote %q while its assets are missing", pending.String())
	}

	var ready bytes.Buffer
	if err := runSelect(strings.NewReader(list), &ready, "v0.4.8"); err != nil {
		t.Fatalf("runSelect: %v", err)
	}
	if got := strings.TrimSpace(ready.String()); got != "v0.4.8" {
		t.Errorf("runSelect -tag v0.4.8 wrote %q, want %q", got, "v0.4.8")
	}
}

// TestGeneratedManifestDrivesSelfUpdate closes the loop that has never closed
// in production: the manifest this command generates is served over HTTPS and
// read back by update.Check, the same call `br self-update --check` makes. It
// covers the whole pipeline except the two halves that need real
// infrastructure — the GitHub release list (a fixture here) and the Pages host
// (an httptest server here).
func TestGeneratedManifestDrivesSelfUpdate(t *testing.T) {
	releases := releaseListJSON(t, []update.Release{
		{TagName: "guest-image-v2026.08.05"},
		updaterRelease("v0.4.9"),
	})
	var generated bytes.Buffer
	if err := runEmit([]string{"-signature", writeSig(t)}, strings.NewReader(releases), &generated); err != nil {
		t.Fatalf("runEmit: %v", err)
	}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(generated.Bytes())
	}))
	defer srv.Close()

	res, err := update.Check(context.Background(), update.Options{
		CurrentVersion: "0.4.7",
		ManifestURL:    srv.URL,
		HTTPClient:     srv.Client(),
	})
	if err != nil {
		t.Fatalf("update.Check over the generated manifest: %v", err)
	}
	if res.LatestVersion != "0.4.9" {
		t.Errorf("LatestVersion = %q, want %q", res.LatestVersion, "0.4.9")
	}
	if !res.UpdateAvailable {
		t.Error("UpdateAvailable = false; 0.4.9 is newer than the running 0.4.7")
	}
}

// TestNoManifestReadsAsNoChannel is the other end of the graceful no-op: when
// no release carries the updater assets the site publishes no latest.json, the
// URL answers 404, and `br self-update` must call that an absent update channel.
func TestNoManifestReadsAsNoChannel(t *testing.T) {
	releases := releaseListJSON(t, []update.Release{{TagName: "v0.4.7"}})
	var out bytes.Buffer
	if err := runSelect(strings.NewReader(releases), &out, ""); err != nil {
		t.Fatalf("runSelect: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("runSelect chose %q; the fixture has no updater assets", out.String())
	}

	// Nothing was written, so the site serves nothing at that path.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer srv.Close()

	_, err := update.Check(context.Background(), update.Options{
		CurrentVersion: "0.4.7",
		ManifestURL:    srv.URL,
		HTTPClient:     srv.Client(),
	})
	if !errors.Is(err, update.ErrNoUpdateChannel) {
		t.Fatalf("update.Check error = %v, want ErrNoUpdateChannel", err)
	}
}

// TestRunSignatureName gives the site build the asset name to download without
// repeating it in the workflow, where nothing would notice it going stale.
func TestRunSignatureName(t *testing.T) {
	var out bytes.Buffer
	if err := runSignatureName(&out); err != nil {
		t.Fatalf("runSignatureName: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != update.UpdaterSignatureName {
		t.Errorf("runSignatureName wrote %q, want %q", got, update.UpdaterSignatureName)
	}
}
