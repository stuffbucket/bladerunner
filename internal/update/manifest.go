package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/blang/semver/v4"
)

// DefaultManifestURL is the HTTPS location of the update manifest. It is a
// package variable (not a const) so tests and the --manifest flag can point at
// an httptest server. The production value publishes alongside the release
// artifacts.
var DefaultManifestURL = "https://stuffbucket.co/bladerunner/latest.json"

// manifestTimeout bounds the manifest fetch so a hung server can't wedge the
// command.
const manifestTimeout = 30 * time.Second

// maxManifestBytes caps the manifest body we read to keep a hostile or
// misconfigured endpoint from exhausting memory.
const maxManifestBytes = 1 << 20 // 1 MiB

// Manifest describes an available release. It intentionally mirrors the small
// subset of the tauri "latest.json" shape that Bladerunner needs: a version, a
// download URL for the signed .app.tar.gz, and the minisign signature string.
type Manifest struct {
	// Version is the release version, e.g. "0.4.8" (with or without a leading
	// "v"; comparisons are tolerant).
	Version string `json:"version"`
	// URL is the HTTPS location of the signed Bladerunner.app.tar.gz.
	URL string `json:"url"`
	// Signature is the base64 minisign .sig content for the tarball, exactly as
	// emitted by macos-builder's updater artifact.
	Signature string `json:"signature"`
	// Notes is optional human-readable release notes.
	Notes string `json:"notes,omitempty"`
	// PubDate is optional and unused by verification.
	PubDate string `json:"pub_date,omitempty"`
}

// validate rejects a manifest that is missing required fields or that points at
// a non-HTTPS URL. Refusing plaintext URLs keeps the download on a channel we
// can reason about (the signature is the real integrity guarantee, but HTTPS
// avoids trivial substitution and metadata leakage).
func (m *Manifest) validate() error {
	if strings.TrimSpace(m.Version) == "" {
		return fmt.Errorf("update: manifest missing version")
	}
	if strings.TrimSpace(m.Signature) == "" {
		return fmt.Errorf("update: manifest missing signature")
	}
	if err := requireHTTPS(m.URL); err != nil {
		return fmt.Errorf("update: manifest artifact url: %w", err)
	}
	return nil
}

// requireHTTPS parses raw and returns an error unless it is a valid absolute
// https:// URL with a host.
func requireHTTPS(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("must be https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("missing host")
	}
	return nil
}

// fetchManifest downloads and parses the manifest at rawURL over HTTPS. The
// URL must itself be https to avoid a downgrade before the signature is even in
// hand.
func fetchManifest(ctx context.Context, client *http.Client, rawURL string) (*Manifest, error) {
	if err := requireHTTPS(rawURL); err != nil {
		return nil, fmt.Errorf("update: manifest url: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, manifestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("update: build manifest request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("update: fetch manifest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("update: manifest fetch: unexpected status %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes))
	if err != nil {
		return nil, fmt.Errorf("update: read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("update: parse manifest: %w", err)
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// isNewer reports whether the manifest version is strictly greater than the
// running version. Parsing is tolerant of a leading "v" and missing patch
// components. A "dev"/unparseable running version is treated as always
// out-of-date so a developer build can still exercise the flow, while an
// unparseable manifest version is treated as not-newer (fail safe: never offer
// a version we cannot compare).
func isNewer(manifestVer, runningVer string) (bool, error) {
	mv, err := semver.ParseTolerant(manifestVer)
	if err != nil {
		return false, fmt.Errorf("update: unparseable manifest version %q: %w", manifestVer, err)
	}
	rv, rvErr := semver.ParseTolerant(runningVer)
	if rvErr != nil {
		// Unknown/dev running version: any real release is "newer". This is a
		// deliberate outcome, not a swallowed failure (nolint:nilerr).
		return true, nil //nolint:nilerr // intentional: an unparseable running version is treated as out-of-date
	}
	return mv.GT(rv), nil
}
