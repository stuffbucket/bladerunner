package update

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/blang/semver/v4"
)

// UpdaterTarballName is the file name of the signed updater artifact that
// stuffbucket/macos-builder uploads onto a product release. The name comes from
// the builder's .macos-builder/config `updater_name`.
const UpdaterTarballName = "Bladerunner.app.tar.gz"

// UpdaterSignatureName is the minisign signature file that accompanies
// UpdaterTarballName. The builder writes it beside the tarball.
const UpdaterSignatureName = UpdaterTarballName + ".sig"

// tagPrefix is the leading character of every product release tag.
const tagPrefix = "v"

// ErrNoUpdaterRelease reports that no release can serve an update: none of them
// is a published product release carrying both updater assets. This is the
// ordinary state of a repository that has not shipped a signed bundle yet, so a
// caller must treat it as a no-op rather than a failure.
var ErrNoUpdaterRelease = errors.New("update: no release carries the updater assets")

// ErrDowngrade reports that a candidate version is older than the version the
// published manifest already advertises. Publishing it would move every user
// backward, so the manifest builder refuses.
var ErrDowngrade = errors.New("update: candidate version is older than the published manifest")

// ReleaseAsset is one file attached to a GitHub release. The field names match
// the GitHub release API, so a `gh api .../releases` document decodes directly.
type ReleaseAsset struct {
	// Name is the asset file name, for example "Bladerunner.app.tar.gz".
	Name string `json:"name"`
	// DownloadURL is the public https location of the asset.
	DownloadURL string `json:"browser_download_url"`
}

// Release is the subset of a GitHub release that the manifest builder reads.
// The field names match the GitHub release API.
type Release struct {
	// TagName is the git tag, for example "v0.4.8".
	TagName string `json:"tag_name"`
	// Name is the release title, used as the manifest notes.
	Name string `json:"name"`
	// Draft reports that the release is not visible to users.
	Draft bool `json:"draft"`
	// Prerelease reports that the release is not on the stable channel.
	Prerelease bool `json:"prerelease"`
	// PublishedAt is the RFC 3339 publication time.
	PublishedAt string `json:"published_at"`
	// Assets are the files attached to the release.
	Assets []ReleaseAsset `json:"assets"`
}

// ProductVersion parses tag as a product release version. A product tag is a
// leading "v" followed by a strict semantic version, which is what release-please
// creates. Every other tag in this repository — the "guest-image-*" tags in
// particular — is refused, because such a release can never carry an updater
// artifact.
func ProductVersion(tag string) (semver.Version, error) {
	rest, ok := strings.CutPrefix(tag, tagPrefix)
	if !ok {
		return semver.Version{}, fmt.Errorf("update: tag %q is not a product release tag", tag)
	}
	v, err := semver.Parse(rest)
	if err != nil {
		return semver.Version{}, fmt.Errorf("update: tag %q is not a product release tag: %w", tag, err)
	}
	return v, nil
}

// asset returns the named asset of the release.
func (r *Release) asset(name string) (ReleaseAsset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return ReleaseAsset{}, false
}

// servesUpdate reports whether the release can serve `br self-update`: it is a
// published, stable product release and both updater assets are attached over
// https.
func (r *Release) servesUpdate() bool {
	if r.Draft || r.Prerelease {
		return false
	}
	if _, err := ProductVersion(r.TagName); err != nil {
		return false
	}
	for _, name := range [...]string{UpdaterTarballName, UpdaterSignatureName} {
		a, ok := r.asset(name)
		if !ok {
			return false
		}
		if err := requireHTTPS(a.DownloadURL); err != nil {
			return false
		}
	}
	return true
}

// SelectRelease returns the highest-versioned release that can serve
// `br self-update`. It compares semantic versions rather than publication
// times, so a guest image published after a product release cannot displace it
// and a late republish of an old tag cannot either.
//
// It returns ErrNoUpdaterRelease when no release qualifies. That is not a
// failure: it is what a repository looks like before its first signed bundle
// ships, and the caller must publish no manifest rather than a broken one.
func SelectRelease(releases []Release) (Release, error) {
	var best Release
	var bestVer semver.Version
	found := false
	for _, r := range releases {
		if !r.servesUpdate() {
			continue
		}
		v, err := ProductVersion(r.TagName)
		if err != nil {
			continue
		}
		if !found || v.GT(bestVer) {
			best, bestVer, found = r, v, true
		}
	}
	if !found {
		return Release{}, ErrNoUpdaterRelease
	}
	return best, nil
}

// BuildManifest converts a release and the contents of its minisign signature
// file into the manifest that `br self-update` fetches.
//
// sigFile is the whole .sig file, and the manifest carries base64 of it. That
// is what parseSignature expects on the reading side, and it matches the tauri
// updater convention.
func BuildManifest(rel Release, sigFile []byte) (*Manifest, error) {
	version, err := ProductVersion(rel.TagName)
	if err != nil {
		return nil, err
	}
	if len(sigFile) == 0 {
		return nil, fmt.Errorf("update: empty signature file for %s", rel.TagName)
	}
	tarball, ok := rel.asset(UpdaterTarballName)
	if !ok {
		return nil, fmt.Errorf("update: release %s has no %s asset", rel.TagName, UpdaterTarballName)
	}
	notes := rel.Name
	if strings.TrimSpace(notes) == "" {
		notes = "Bladerunner " + rel.TagName
	}
	m := &Manifest{
		Version:   version.String(),
		URL:       tarball.DownloadURL,
		Signature: base64.StdEncoding.EncodeToString(sigFile),
		Notes:     notes,
		PubDate:   rel.PublishedAt,
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return m, nil
}

// publishedVersion reads the version a published manifest advertises. An empty
// or unreadable value reports false: there is nothing to protect, so the caller
// publishes over it.
func publishedVersion(live string) (semver.Version, bool) {
	v, err := semver.ParseTolerant(live)
	if err != nil {
		return semver.Version{}, false
	}
	return v, true
}

// CheckDowngrade returns ErrDowngrade when candidate is an older version than
// live, the version the published manifest currently advertises. An empty or
// unreadable live version is no constraint.
//
// A candidate that cannot be read is a separate error. The builder never
// publishes a version it cannot compare.
func CheckDowngrade(candidate, live string) error {
	cv, err := semver.ParseTolerant(candidate)
	if err != nil {
		return fmt.Errorf("update: unreadable candidate version %q: %w", candidate, err)
	}
	lv, ok := publishedVersion(live)
	if !ok {
		return nil
	}
	if cv.LT(lv) {
		return fmt.Errorf("%w: %s is older than the published %s", ErrDowngrade, cv, lv)
	}
	return nil
}
