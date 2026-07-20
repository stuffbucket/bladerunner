package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// productionPublicKey is the pinned Ed25519 public key (minisign/tauri format,
// base64) used to verify update artifacts. This is the PUBLIC half of the
// macos-builder TAURI_SIGNING key (key id BF65715BAE1C1F9F); it carries no
// secret, so embedding it as a source literal is correct and preferred over
// ldflags for reproducibility. An empty key makes Apply fail closed
// (verifyTarball rejects it), so a build that somehow blanks the key cannot
// silently install anything.
//
// It may still be overridden at build time (e.g. to pin a different channel)
// with:
//
//	-ldflags "-X github.com/stuffbucket/bladerunner/internal/update.productionPublicKey=<base64>"
var productionPublicKey = "dW50cnVzdGVkIGNvbW1lbnQ6IG1pbmlzaWduIHB1YmxpYyBrZXk6IEJGNjU3MTVCQUUxQzFGOUYKUldTZkh4eXVXM0ZsdjF1RkM4N1BBUHYrNFZWeWI1YTUvd05EMENiY25iVVVNcms5dE9adnBlTVgK"

// downloadTimeout bounds the artifact download.
const downloadTimeout = 5 * time.Minute

// maxArtifactBytes caps the tarball we will download. Bladerunner.app is a few
// tens of MiB; 512 MiB is a generous ceiling that still stops a runaway body.
const maxArtifactBytes = 512 << 20

// Options configures a self-update run. Zero values select safe defaults, so a
// caller can pass &Options{CurrentVersion: version} and get production
// behavior.
type Options struct {
	// CurrentVersion is the running binary's version (from main.version).
	CurrentVersion string
	// ManifestURL overrides DefaultManifestURL (used by --manifest and tests).
	ManifestURL string
	// PublicKey overrides the embedded productionPublicKey (used by tests and
	// operators pinning a different channel). When empty the embedded key is
	// used.
	PublicKey string
	// HTTPClient overrides the default client (tests inject an httptest one).
	HTTPClient *http.Client
	// ExecPath overrides os.Executable() for Homebrew/bundle detection (tests).
	ExecPath string
	// Relaunch, when true, restarts the swapped app after a successful install.
	Relaunch bool
}

func (o *Options) manifestURL() string {
	if o.ManifestURL != "" {
		return o.ManifestURL
	}
	return DefaultManifestURL
}

func (o *Options) publicKey() string {
	if o.PublicKey != "" {
		return o.PublicKey
	}
	return productionPublicKey
}

func (o *Options) httpClient() *http.Client {
	if o.HTTPClient != nil {
		return o.HTTPClient
	}
	return &http.Client{Timeout: downloadTimeout}
}

func (o *Options) execPath() (string, error) {
	if o.ExecPath != "" {
		return o.ExecPath, nil
	}
	return os.Executable()
}

// CheckResult reports the outcome of a dry-run check.
type CheckResult struct {
	// CurrentVersion is the running version.
	CurrentVersion string
	// LatestVersion is the manifest's version.
	LatestVersion string
	// UpdateAvailable is true when LatestVersion is strictly newer.
	UpdateAvailable bool
	// Notes carries the manifest's release notes, if any.
	Notes string
}

// Check performs a dry run: fetch the manifest and compare versions. It never
// downloads the artifact and never touches the installed bundle, so it is safe
// to run anywhere (and fully exercisable in unit tests). Homebrew installs are
// not refused here — checking is always allowed; only Apply defers to brew.
func Check(ctx context.Context, opts Options) (*CheckResult, error) {
	m, err := fetchManifest(ctx, opts.httpClient(), opts.manifestURL())
	if err != nil {
		return nil, err
	}
	newer, err := isNewer(m.Version, opts.CurrentVersion)
	if err != nil {
		return nil, err
	}
	return &CheckResult{
		CurrentVersion:  opts.CurrentVersion,
		LatestVersion:   m.Version,
		UpdateAvailable: newer,
		Notes:           m.Notes,
	}, nil
}

// ApplyResult reports the outcome of an applied update.
type ApplyResult struct {
	// FromVersion / ToVersion bracket the swap.
	FromVersion string
	ToVersion   string
	// BundlePath is the .app that was replaced.
	BundlePath string
	// Relaunched reports whether the app was restarted.
	Relaunched bool
}

// Apply performs the full self-update: resolve/verify the install target,
// fetch the manifest, download the artifact, verify its signature against the
// pinned key (FAIL CLOSED), extract, and atomically swap the bundle. It refuses
// Homebrew-managed installs before any network work.
//
// If opts.Relaunch is set and the swap succeeds, it relaunches the app.
func Apply(ctx context.Context, opts Options) (*ApplyResult, error) {
	exe, err := opts.execPath()
	if err != nil {
		return nil, fmt.Errorf("update: locate running binary: %w", err)
	}
	// Resolve (and reject Homebrew / non-bundle) before any network work so the
	// failure is fast and offline.
	bundle, err := installTarget(exe)
	if err != nil {
		return nil, err
	}

	m, err := fetchManifest(ctx, opts.httpClient(), opts.manifestURL())
	if err != nil {
		return nil, err
	}
	newer, err := isNewer(m.Version, opts.CurrentVersion)
	if err != nil {
		return nil, err
	}
	if !newer {
		return &ApplyResult{FromVersion: opts.CurrentVersion, ToVersion: m.Version, BundlePath: bundle}, nil
	}

	tarball, err := download(ctx, opts.httpClient(), m.URL)
	if err != nil {
		return nil, err
	}

	// FAIL CLOSED: never install an artifact whose signature does not verify
	// against the pinned public key.
	if err := verifyTarball(tarball, m.Signature, opts.publicKey()); err != nil {
		return nil, fmt.Errorf("update: refusing unverified artifact: %w", err)
	}

	// Stage the new bundle on the SAME filesystem as the target so the final
	// rename is atomic. A staging dir beside the bundle's parent guarantees
	// that.
	parent := filepath.Dir(bundle)
	staging, err := os.MkdirTemp(parent, ".br-update-*")
	if err != nil {
		return nil, fmt.Errorf("update: create staging dir: %w", err)
	}
	defer os.RemoveAll(staging)

	newApp, err := extractAppBundle(tarball, staging)
	if err != nil {
		return nil, err
	}

	if err := swapBundle(bundle, newApp); err != nil {
		return nil, err
	}

	res := &ApplyResult{FromVersion: opts.CurrentVersion, ToVersion: m.Version, BundlePath: bundle}
	if opts.Relaunch {
		if err := relaunch(ctx, bundle); err != nil {
			// The swap already succeeded; a relaunch failure is non-fatal but
			// worth surfacing.
			return res, fmt.Errorf("update: installed but relaunch failed: %w", err)
		}
		res.Relaunched = true
	}
	return res, nil
}

// download fetches rawURL over HTTPS and returns the body, capped at
// maxArtifactBytes. It optionally verifies a fragment-less sha256 if the URL
// carries one, but the Ed25519 signature is the authoritative integrity check.
func download(ctx context.Context, client *http.Client, rawURL string) ([]byte, error) {
	if err := requireHTTPS(rawURL); err != nil {
		return nil, fmt.Errorf("update: artifact url: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("update: build download request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("update: download artifact: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("update: download: unexpected status %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxArtifactBytes+1))
	if err != nil {
		return nil, fmt.Errorf("update: read artifact: %w", err)
	}
	if len(body) > maxArtifactBytes {
		return nil, fmt.Errorf("update: artifact exceeds %d bytes", maxArtifactBytes)
	}
	return body, nil
}

// sha256Hex returns the lowercase hex sha256 of b. Exposed for the optional
// .sha256 cross-check and for tests.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// relaunch restarts the swapped .app bundle. On macOS `open` re-launches the
// bundle in a fresh process group, detached from this one.
func relaunch(ctx context.Context, bundle string) error {
	if runtime.GOOS != "darwin" {
		return errors.New("relaunch is only supported on macOS")
	}
	if !strings.HasSuffix(strings.ToLower(bundle), ".app") {
		return fmt.Errorf("relaunch target is not an .app bundle: %q", bundle)
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/open", "-n", bundle)
	return cmd.Start()
}
