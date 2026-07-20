package update

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// TestManifestContract guards the JSON contract that
// .github/workflows/publish-update-manifest.yml emits into site/public/latest.json.
// It constructs a manifest exactly as the workflow would (version derived from
// the tag without a leading "v"; url is the https GitHub release download link;
// signature is base64 of the whole minisign .sig file text) and asserts it
// round-trips through the real Manifest parse + validate path. If the Go struct
// or validation ever drifts from the workflow's shape, this fails.
func TestManifestContract(t *testing.T) {
	// Build a realistic signature the way the workflow does: base64 of the whole
	// minisign .sig file. kp.sign already returns exactly that form (see
	// verify_test.go), which is what `base64 -w0 Bladerunner.app.tar.gz.sig`
	// produces from tauri's .sig output. Using the real signer keeps the fixture
	// honest — the same string also parses via parseSignature below.
	kp := newTestKeypair(t)
	data := []byte("fake Bladerunner.app.tar.gz payload")
	signature := kp.sign(data, "timestamp:1720000000\tfile:Bladerunner.app.tar.gz")

	// The workflow strips a leading "v" from the tag for `version` and uses the
	// asset's browser_download_url for `url`.
	const tag = "v0.4.8"
	wantVersion := strings.TrimPrefix(tag, "v")
	url := "https://github.com/stuffbucket/bladerunner/releases/download/" + tag + "/Bladerunner.app.tar.gz"

	// Emit the manifest JSON with the same keys `jq -n` writes in the workflow.
	emitted := map[string]string{
		"version":   wantVersion,
		"url":       url,
		"signature": signature,
		"notes":     "Bladerunner " + tag,
		"pub_date":  "2026-07-20T00:00:00Z",
	}
	raw, err := json.Marshal(emitted)
	if err != nil {
		t.Fatalf("marshal emitted manifest: %v", err)
	}

	// Parse through the real Manifest struct + validate(), the exact path
	// fetchManifest uses after downloading latest.json.
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal into Manifest: %v", err)
	}
	if err := m.validate(); err != nil {
		t.Fatalf("workflow-shaped manifest failed validate(): %v", err)
	}

	if m.Version != wantVersion {
		t.Errorf("version = %q, want %q (leading v must be stripped)", m.Version, wantVersion)
	}
	if !strings.HasPrefix(m.URL, "https://") {
		t.Errorf("url = %q, want https:// prefix", m.URL)
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
	// A raw (un-base64'd) .sig would fail here, catching a workflow regression.
	if _, err := base64.StdEncoding.DecodeString(m.Signature); err != nil {
		t.Fatalf("signature is not valid base64 (workflow must base64 the .sig file): %v", err)
	}
	if _, err := parseSignature(m.Signature); err != nil {
		t.Fatalf("signature does not parse as a minisign .sig: %v", err)
	}
}

// TestManifestContract_RejectsRawSigText asserts the negative: if the workflow
// mistakenly emitted the raw multi-line .sig text instead of base64 of it, the
// updater would reject it. This documents why the workflow base64-encodes.
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
