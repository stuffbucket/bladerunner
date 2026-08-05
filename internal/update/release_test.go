package update_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/update"
)

// downloadBase is the prefix of every GitHub release asset download URL this
// repository produces.
const downloadBase = "https://github.com/stuffbucket/bladerunner/releases/download/"

// updaterAssets returns the pair of assets that stuffbucket/macos-builder
// uploads onto a product release, as the GitHub release API reports them.
func updaterAssets(tag string) []update.ReleaseAsset {
	return []update.ReleaseAsset{
		{Name: update.UpdaterTarballName, DownloadURL: downloadBase + tag + "/" + update.UpdaterTarballName},
		{Name: update.UpdaterSignatureName, DownloadURL: downloadBase + tag + "/" + update.UpdaterSignatureName},
	}
}

// guestImageAssets returns the assets a guest-image release carries. None of
// them is an updater artifact.
func guestImageAssets() []update.ReleaseAsset {
	return []update.ReleaseAsset{
		{Name: "bladerunner-guest-arm64.qcow2", DownloadURL: downloadBase + "guest-image-v2026.08.05/bladerunner-guest-arm64.qcow2"},
	}
}

// TestSelectRelease picks the highest product release that carries both updater
// assets, and ignores every release that cannot serve an update.
func TestSelectRelease(t *testing.T) {
	tests := []struct {
		name     string
		releases []update.Release
		want     string
		wantErr  error
	}{
		{
			name:     "no releases at all",
			releases: nil,
			wantErr:  update.ErrNoUpdaterRelease,
		},
		{
			name: "guest image releases never qualify",
			releases: []update.Release{
				{TagName: "guest-image-v2026.08.05", Assets: guestImageAssets()},
				{TagName: "guest-image-latest", Assets: guestImageAssets()},
			},
			wantErr: update.ErrNoUpdaterRelease,
		},
		{
			name: "product release without updater assets does not qualify",
			releases: []update.Release{
				{TagName: "v0.4.7", Assets: []update.ReleaseAsset{
					{Name: "bladerunner_0.4.7_darwin_aarch64.dmg", DownloadURL: downloadBase + "v0.4.7/bladerunner_0.4.7_darwin_aarch64.dmg"},
				}},
			},
			wantErr: update.ErrNoUpdaterRelease,
		},
		{
			name: "the signature alone is not enough",
			releases: []update.Release{
				{TagName: "v0.4.8", Assets: []update.ReleaseAsset{
					{Name: update.UpdaterSignatureName, DownloadURL: downloadBase + "v0.4.8/" + update.UpdaterSignatureName},
				}},
			},
			wantErr: update.ErrNoUpdaterRelease,
		},
		{
			name: "highest version wins, not the most recently published",
			releases: []update.Release{
				{TagName: "v0.4.9", PublishedAt: "2026-01-01T00:00:00Z", Assets: updaterAssets("v0.4.9")},
				{TagName: "v0.4.10", PublishedAt: "2025-01-01T00:00:00Z", Assets: updaterAssets("v0.4.10")},
			},
			want: "v0.4.10",
		},
		{
			name: "a draft release is invisible to users",
			releases: []update.Release{
				{TagName: "v0.5.0", Draft: true, Assets: updaterAssets("v0.5.0")},
				{TagName: "v0.4.8", Assets: updaterAssets("v0.4.8")},
			},
			want: "v0.4.8",
		},
		{
			name: "a prerelease is not offered on the stable channel",
			releases: []update.Release{
				{TagName: "v0.5.0-rc.1", Prerelease: true, Assets: updaterAssets("v0.5.0-rc.1")},
				{TagName: "v0.4.8", Assets: updaterAssets("v0.4.8")},
			},
			want: "v0.4.8",
		},
		{
			name: "an asset served over http is refused",
			releases: []update.Release{
				{TagName: "v0.4.8", Assets: []update.ReleaseAsset{
					{Name: update.UpdaterTarballName, DownloadURL: "http://example.invalid/" + update.UpdaterTarballName},
					{Name: update.UpdaterSignatureName, DownloadURL: downloadBase + "v0.4.8/" + update.UpdaterSignatureName},
				}},
			},
			wantErr: update.ErrNoUpdaterRelease,
		},
		{
			name: "the real release list of this repository has no updater release yet",
			releases: []update.Release{
				{TagName: "guest-image-v2026.08.05", Assets: guestImageAssets()},
				{TagName: "v0.4.7", Assets: []update.ReleaseAsset{
					{Name: "bladerunner_0.4.7_darwin_aarch64.dmg", DownloadURL: downloadBase + "v0.4.7/bladerunner_0.4.7_darwin_aarch64.dmg"},
					{Name: "checksums.txt", DownloadURL: downloadBase + "v0.4.7/checksums.txt"},
				}},
			},
			wantErr: update.ErrNoUpdaterRelease,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := update.SelectRelease(tt.releases)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("SelectRelease error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("SelectRelease: %v", err)
			}
			if got.TagName != tt.want {
				t.Errorf("SelectRelease tag = %q, want %q", got.TagName, tt.want)
			}
		})
	}
}

// TestBuildManifest builds a manifest from a release and asserts every field
// the updater reads.
func TestBuildManifest(t *testing.T) {
	sig := []byte("untrusted comment: signature\nRWSfHxyuW3Flv...\ntrusted comment: x\nabc\n")
	rel := update.Release{
		TagName:     "v0.4.8",
		Name:        "bladerunner v0.4.8",
		PublishedAt: "2026-08-05T00:00:00Z",
		Assets:      updaterAssets("v0.4.8"),
	}

	m, err := update.BuildManifest(rel, sig)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if m.Version != "0.4.8" {
		t.Errorf("version = %q, want %q (the leading v must be stripped)", m.Version, "0.4.8")
	}
	if want := downloadBase + "v0.4.8/" + update.UpdaterTarballName; m.URL != want {
		t.Errorf("url = %q, want %q", m.URL, want)
	}
	if want := base64.StdEncoding.EncodeToString(sig); m.Signature != want {
		t.Errorf("signature = %q, want base64 of the whole .sig file", m.Signature)
	}
	if m.Notes != rel.Name {
		t.Errorf("notes = %q, want %q", m.Notes, rel.Name)
	}
	if m.PubDate != rel.PublishedAt {
		t.Errorf("pub_date = %q, want %q", m.PubDate, rel.PublishedAt)
	}
}

// TestBuildManifest_NamelessRelease falls back to a readable note when the
// release has no title.
func TestBuildManifest_NamelessRelease(t *testing.T) {
	rel := update.Release{TagName: "v0.4.8", Assets: updaterAssets("v0.4.8")}
	m, err := update.BuildManifest(rel, []byte("sig"))
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if m.Notes == "" {
		t.Error("notes is empty; a nameless release must still get a note")
	}
}

// TestBuildManifest_Rejects covers every input BuildManifest must refuse.
func TestBuildManifest_Rejects(t *testing.T) {
	tests := []struct {
		name string
		rel  update.Release
		sig  []byte
	}{
		{
			name: "empty signature file",
			rel:  update.Release{TagName: "v0.4.8", Assets: updaterAssets("v0.4.8")},
			sig:  nil,
		},
		{
			name: "no updater tarball on the release",
			rel:  update.Release{TagName: "v0.4.8", Assets: guestImageAssets()},
			sig:  []byte("sig"),
		},
		{
			name: "tag that is not a product version",
			rel:  update.Release{TagName: "guest-image-v2026.08.05", Assets: updaterAssets("guest-image-v2026.08.05")},
			sig:  []byte("sig"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := update.BuildManifest(tt.rel, tt.sig); err == nil {
				t.Fatal("BuildManifest accepted an input it must refuse")
			}
		})
	}
}

// TestCheckDowngrade holds the rule salvaged from the abandoned publish
// workflow: a manifest may never advertise a version older than the one already
// published, because `br self-update` would push every user backward.
func TestCheckDowngrade(t *testing.T) {
	tests := []struct {
		name      string
		candidate string
		live      string
		wantErr   bool
	}{
		{name: "nothing published yet", candidate: "0.4.8", live: "", wantErr: false},
		{name: "same version republished", candidate: "0.4.8", live: "0.4.8", wantErr: false},
		{name: "newer version", candidate: "0.4.9", live: "0.4.8", wantErr: false},
		{name: "double-digit patch is newer", candidate: "0.4.10", live: "0.4.9", wantErr: false},
		{name: "older version refused", candidate: "0.4.7", live: "0.4.8", wantErr: true},
		{name: "older double-digit patch refused", candidate: "0.4.9", live: "0.4.10", wantErr: true},
		{name: "leading v is tolerated on both sides", candidate: "v0.4.9", live: "v0.4.8", wantErr: false},
		{name: "a prerelease is older than its release", candidate: "0.5.0-rc.1", live: "0.5.0", wantErr: true},
		{name: "a release supersedes its prerelease", candidate: "0.5.0", live: "0.5.0-rc.1", wantErr: false},
		{name: "an unreadable live version does not block publishing", candidate: "0.4.8", live: "garbage", wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := update.CheckDowngrade(tt.candidate, tt.live)
			if tt.wantErr {
				if !errors.Is(err, update.ErrDowngrade) {
					t.Fatalf("CheckDowngrade(%q, %q) = %v, want ErrDowngrade", tt.candidate, tt.live, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("CheckDowngrade(%q, %q) = %v, want nil", tt.candidate, tt.live, err)
			}
		})
	}
}

// TestCheckDowngrade_UnreadableCandidate refuses to publish a version it cannot
// compare, rather than assuming it is newer.
func TestCheckDowngrade_UnreadableCandidate(t *testing.T) {
	err := update.CheckDowngrade("not-a-version", "0.4.8")
	if err == nil {
		t.Fatal("CheckDowngrade accepted an unreadable candidate version")
	}
	if errors.Is(err, update.ErrDowngrade) {
		t.Error("an unreadable candidate must not be reported as a downgrade")
	}
}

// TestReleaseRoundTrip holds the JSON field names against the GitHub release
// API. The generator decodes the output of `gh api .../releases` into these
// structs, so a renamed tag silently empties a field and publishes a broken
// manifest. The document below is a trimmed copy of a real API response.
func TestReleaseRoundTrip(t *testing.T) {
	const apiResponse = `[
	  {
	    "tag_name": "v0.4.8",
	    "name": "bladerunner v0.4.8",
	    "draft": false,
	    "prerelease": false,
	    "published_at": "2026-08-05T00:53:14Z",
	    "assets": [
	      {
	        "name": "Bladerunner.app.tar.gz",
	        "browser_download_url": "https://github.com/stuffbucket/bladerunner/releases/download/v0.4.8/Bladerunner.app.tar.gz"
	      }
	    ]
	  }
	]`

	var got []update.Release
	if err := json.Unmarshal([]byte(apiResponse), &got); err != nil {
		t.Fatalf("unmarshal release list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("decoded %d releases, want 1", len(got))
	}
	want := update.Release{
		TagName:     "v0.4.8",
		Name:        "bladerunner v0.4.8",
		Draft:       false,
		Prerelease:  false,
		PublishedAt: "2026-08-05T00:53:14Z",
		Assets: []update.ReleaseAsset{{
			Name:        update.UpdaterTarballName,
			DownloadURL: downloadBase + "v0.4.8/" + update.UpdaterTarballName,
		}},
	}
	if got[0].TagName != want.TagName || got[0].Name != want.Name ||
		got[0].PublishedAt != want.PublishedAt || got[0].Draft != want.Draft ||
		got[0].Prerelease != want.Prerelease {
		t.Errorf("decoded release = %+v, want %+v", got[0], want)
	}
	if len(got[0].Assets) != 1 || got[0].Assets[0] != want.Assets[0] {
		t.Errorf("decoded assets = %+v, want %+v", got[0].Assets, want.Assets)
	}

	// Re-encode and decode again: the struct must survive its own output, which
	// is what lets a test fixture stand in for the live API.
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal releases: %v", err)
	}
	var round []update.Release
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatalf("unmarshal round trip: %v", err)
	}
	if round[0].TagName != want.TagName || len(round[0].Assets) != 1 ||
		round[0].Assets[0] != want.Assets[0] {
		t.Errorf("round trip = %+v, want %+v", round[0], want)
	}
}
