package imagebuild

import (
	"bufio"
	"context"
	"crypto/sha512"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stuffbucket/bladerunner/internal/httpfetch"
)

// Where the guest image starts from.
//
// The stamp names an immutable dated directory rather than "latest". A build
// from an old commit then gets the bytes that commit was tested against, and a
// Debian rebuild cannot change what an existing tag produces. Moving to a newer
// Debian means editing this constant and the pins together, in one reviewed
// commit.
const (
	debianSuite   = "trixie"
	debianStamp   = "20260722-2547"
	debianRelease = "13"
	debianVariant = "genericcloud"
	debianBaseURL = "https://cloud.debian.org/images/cloud"
	// manifestName is the checksum list Debian publishes beside its images.
	manifestName = "SHA512SUMS"
)

// downloadTimeout bounds a base image fetch. The images are a few hundred
// megabytes, so this is generous rather than tight; its job is to stop a
// wedged connection from hanging a build forever.
const downloadTimeout = 30 * time.Minute

// downloadStallTimeout bounds time WITHOUT PROGRESS inside that window. The
// outer deadline alone lets a peer trickle one byte an hour for half an hour
// before anything notices; this reports the wedge in a minute and says which
// kind of failure it was. It is a var only so a test can shorten it.
var downloadStallTimeout = httpfetch.StallTimeout

// Manifest scanning limits. Debian writes one short line per file, but a long
// line should widen the buffer rather than silently truncate the map.
const (
	manifestScanInitial = 64 * 1024
	manifestScanMax     = 1024 * 1024
)

// basePinsFileName is the reviewed-digest file, named once so the shell build
// and the tests that hold the two paths together refer to the same thing. The
// //go:embed directive below needs a literal, so this constant cannot feed it;
// TestBasePinsFileNameMatchesTheEmbed keeps the two honest.
const basePinsFileName = "basepins.sha512"

//go:embed basepins.sha512
var basePinsRaw string

// basePins maps a release filename to its reviewed SHA-512. It is a variable
// rather than a constant so a test can pin a synthetic image instead of
// downloading hundreds of megabytes.
var basePins = parsePins(basePinsRaw)

// ErrUnpinnedRelease reports a release with no reviewed digest.
var ErrUnpinnedRelease = errors.New("no pinned digest for this release")

// Release identifies one immutable Debian cloud image.
type Release struct {
	// Suite is the Debian suite, such as "trixie".
	Suite string
	// Stamp is the dated build directory, such as "20260722-2547".
	Stamp string
	// Arch is the Debian architecture name, such as "arm64".
	Arch string
}

// BaseRelease returns the pinned Debian release for a target architecture.
//
// An architecture is supported when — and only when — basepins.sha512 carries a
// digest for it. The set is derived rather than listed, so adding one is a
// single edit to the pin file: a hardcoded list beside the pins is a check that
// can only cover what existed when it was written, and the failure it misses is
// always an addition.
func BaseRelease(arch string) (Release, error) {
	r := Release{Suite: debianSuite, Stamp: debianStamp, Arch: arch}
	if _, err := r.PinnedDigest(); err != nil {
		return Release{}, fmt.Errorf("unsupported architecture %q: %w", arch, err)
	}
	return r, nil
}

// FileName is the image's name inside its dated directory.
//
// It differs from the name under "latest": the dated directory carries the
// stamp in the filename too, which is what makes a cached copy self-describing.
func (r Release) FileName() string {
	return fmt.Sprintf("debian-%s-%s-%s-%s.qcow2", debianRelease, debianVariant, r.Arch, r.Stamp)
}

// URL is where the image is fetched from.
func (r Release) URL() string {
	return fmt.Sprintf("%s/%s/%s/%s", debianBaseURL, r.Suite, r.Stamp, r.FileName())
}

// ManifestURL is Debian's checksum list for the same directory.
func (r Release) ManifestURL() string {
	return fmt.Sprintf("%s/%s/%s/%s", debianBaseURL, r.Suite, r.Stamp, manifestName)
}

// PinnedDigest returns the reviewed SHA-512 for the release.
func (r Release) PinnedDigest() (string, error) {
	digest, ok := basePins[r.FileName()]
	if !ok {
		return "", fmt.Errorf("%w: %s is not listed in basepins.sha512", ErrUnpinnedRelease, r.FileName())
	}
	return digest, nil
}

// FetchBase puts the release's image at dest, verified against its pin.
//
// baseURL overrides the Debian mirror; an empty value uses the real one. Only
// tests pass anything else.
//
// A local file that already matches the pin is kept, because these images are
// hundreds of megabytes and a rebuild should not refetch one it has already
// verified. A local file that does NOT match is replaced rather than trusted:
// that is the interrupted-download and tampered-cache case, and reusing it
// would bake an unknown image into every guest.
func FetchBase(ctx context.Context, r Release, dest, baseURL string) error {
	want, err := r.PinnedDigest()
	if err != nil {
		return err
	}

	if matchesDigest(dest, want) {
		return nil
	}

	url := r.URL()
	if baseURL != "" {
		url = strings.TrimSuffix(baseURL, "/") + "/" + r.FileName()
	}

	// Download beside the destination and rename only after the digest checks
	// out, so a partial or wrong image never occupies the name a later step
	// reads.
	if err := os.MkdirAll(filepath.Dir(dest), guestDirMode); err != nil {
		return fmt.Errorf("create the directory for %s: %w", dest, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+".partial-*")
	if err != nil {
		return fmt.Errorf("create a temporary file for the download: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName) // No-op once the rename has succeeded.
	}()

	got, err := downloadTo(ctx, url, tmp)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("digest mismatch for %s\n  expected %s\n  actual   %s\n"+
			"the image at %s is not the reviewed one; refusing to build from it",
			r.FileName(), want, got, url)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close the downloaded image: %w", err)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return fmt.Errorf("move the verified image into place: %w", err)
	}
	return nil
}

// downloadTo streams url into w and returns the SHA-512 of what it wrote.
//
// The digest is computed while streaming rather than by re-reading the file,
// so the bytes that were checked are exactly the bytes that were stored.
//
// Two bounds apply. The outer context deadline caps the whole fetch, and the
// stall watchdog caps how long the peer may send nothing — the second is what
// distinguishes a slow mirror from a wedged one, and it reports in a minute
// rather than in half an hour.
func downloadTo(ctx context.Context, url string, w io.Writer) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()

	resp, err := httpfetch.Get(ctx, httpfetch.StreamingClient(), url, downloadStallTimeout)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch %s: unexpected status %s", url, resp.Status)
	}

	sum := sha512.New()
	if _, err := io.Copy(io.MultiWriter(w, sum), resp.Body); err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// VerifyPinAgainstUpstream checks the reviewed digest against Debian's own
// manifest for the same directory.
//
// This is deliberately NOT the security boundary. Debian publishes no detached
// signature for its cloud images, so the manifest is served by the same host as
// the image and an attacker able to replace one could replace the other. What
// it does catch is a pin that has gone stale — Debian can respin a dated
// directory — which an immutable URL alone cannot detect. A disagreement means
// a human must look, so it is an error rather than a warning.
func VerifyPinAgainstUpstream(ctx context.Context, r Release, baseURL string) error {
	want, err := r.PinnedDigest()
	if err != nil {
		return err
	}

	url := r.ManifestURL()
	if baseURL != "" {
		url = strings.TrimSuffix(baseURL, "/") + "/" + manifestName
	}

	var body strings.Builder
	if _, err := downloadTo(ctx, url, &body); err != nil {
		return fmt.Errorf("fetch the upstream manifest: %w", err)
	}

	upstream, ok := parsePins(body.String())[r.FileName()]
	if !ok {
		return fmt.Errorf("the upstream manifest at %s does not list %s; "+
			"the dated release may have been withdrawn", url, r.FileName())
	}
	if upstream != want {
		return fmt.Errorf("the pinned digest for %s disagrees with upstream\n"+
			"  pinned   %s\n  upstream %s\n"+
			"Debian has respun this dated release. Review the change and update "+
			"basepins.sha512 deliberately; do not copy the new digest blindly",
			r.FileName(), want, upstream)
	}
	return nil
}

// matchesDigest reports whether the file at path already hashes to want.
func matchesDigest(path, want string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	sum := sha512.New()
	if _, err := io.Copy(sum, f); err != nil {
		return false
	}
	return hex.EncodeToString(sum.Sum(nil)) == want
}

// parsePins reads `sha512sum -c` style lines into a filename-to-digest map,
// ignoring comments and blank lines. Debian's own manifest uses this format,
// so the same parser reads both the pins and the upstream list.
func parsePins(text string) map[string]string {
	out := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(text))
	scanner.Buffer(make([]byte, 0, manifestScanInitial), manifestScanMax)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		out[strings.TrimPrefix(fields[1], "*")] = fields[0]
	}
	return out
}
