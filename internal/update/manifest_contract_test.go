package update

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// TestManifestContract guards the JSON contract between the manifest generator
// (BuildManifest, driven by cmd/update-manifest inside pages.yml) and the
// updater that reads it. It runs the real builder over a release shaped like the
// one macos-builder produces, then asserts the output round-trips through the
// real Manifest parse + validate path. Because the generator itself is under
// test here, the workflow cannot drift from the reader without this failing.
func TestManifestContract(t *testing.T) {
	// Build a realistic signature the way macos-builder emits it: kp.sign
	// returns base64 of a whole minisign .sig file (see verify_test.go), which
	// is what tauri writes beside the tarball. Decode it back to the file text,
	// because that is what BuildManifest is handed — the bytes of the .sig file.
	kp := newTestKeypair(t)
	data := []byte("fake Bladerunner.app.tar.gz payload")
	sigFile, err := base64.StdEncoding.DecodeString(kp.sign(data, "timestamp:1720000000\tfile:Bladerunner.app.tar.gz"))
	if err != nil {
		t.Fatalf("decode test signature: %v", err)
	}

	const tag = "v0.4.8"
	wantVersion := strings.TrimPrefix(tag, "v")
	url := "https://github.com/stuffbucket/bladerunner/releases/download/" + tag + "/" + UpdaterTarballName

	rel := Release{
		TagName:     tag,
		Name:        "bladerunner " + tag,
		PublishedAt: "2026-07-20T00:00:00Z",
		Assets: []ReleaseAsset{
			{Name: UpdaterTarballName, DownloadURL: url},
			{Name: UpdaterSignatureName, DownloadURL: url + ".sig"},
		},
	}
	built, err := BuildManifest(rel, sigFile)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}

	// Serialize it exactly as cmd/update-manifest writes latest.json, then read
	// it back through the reader's own path.
	raw, err := json.Marshal(built)
	if err != nil {
		t.Fatalf("marshal built manifest: %v", err)
	}

	// Parse through the real Manifest struct + validate(), the exact path
	// fetchManifest uses after downloading latest.json.
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal into Manifest: %v", err)
	}
	if err := m.validate(); err != nil {
		t.Fatalf("generated manifest failed validate(): %v", err)
	}

	if m.Version != wantVersion {
		t.Errorf("version = %q, want %q (leading v must be stripped)", m.Version, wantVersion)
	}
	if m.URL != url {
		t.Errorf("url = %q, want %q", m.URL, url)
	}
	if m.Signature == "" {
		t.Error("signature is empty")
	}
	if m.Notes == "" {
		t.Error("notes is empty")
	}
	if m.PubDate == "" {
		t.Error("pub_date is empty")
	}

	// The signature field must be base64 of the whole minisign .sig file, not the
	// raw multi-line .sig text. Confirm it decodes as base64 and parses via the
	// real parseSignature — the same decode fetchManifest -> verifyTarball does.
	// A raw (un-base64'd) .sig would fail here, catching a generator regression.
	if _, err := base64.StdEncoding.DecodeString(m.Signature); err != nil {
		t.Fatalf("signature is not valid base64 (the generator must base64 the .sig file): %v", err)
	}
	if _, err := parseSignature(m.Signature); err != nil {
		t.Fatalf("signature does not parse as a minisign .sig: %v", err)
	}
}

// TestManifestContract_RejectsRawSigText asserts the negative: if the generator
// mistakenly emitted the raw multi-line .sig text instead of base64 of it, the
// updater would reject it. This documents why BuildManifest base64-encodes.
func TestManifestContract_RejectsRawSigText(t *testing.T) {
	kp := newTestKeypair(t)
	data := []byte("payload")
	// Decode the correct (base64) signature back to the raw .sig file text.
	good := kp.sign(data, "timestamp:1\tfile:x")
	rawSigText, err := base64.StdEncoding.DecodeString(good)
	if err != nil {
		t.Fatalf("decode good signature: %v", err)
	}

	// A manifest carrying the RAW .sig text (a common mistake) must not parse as a
	// signature — parseSignature base64-decodes its input first.
	if _, err := parseSignature(string(rawSigText)); err == nil {
		t.Fatal("raw .sig text unexpectedly parsed; the manifest MUST carry base64 of the .sig file")
	}
}
