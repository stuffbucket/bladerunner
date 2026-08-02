package imagebuild

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// latestDirectory matches a fetch from Debian's mutable "latest" pointer.
var latestDirectory = regexp.MustCompile(`/images/cloud/[^/"]*/latest/`)

// sha512HexLength is the length of a SHA-512 digest written as hex: 64 bytes,
// two characters each. A pin of any other length is a truncated paste.
const sha512HexLength = 128

// The shell build is what CI actually runs (.github/workflows/build-guest-image.yml
// invokes it directly, not the br binary), so its base image is the base image
// of every published guest.
//
// It must not come from "latest". That directory is a moving pointer: two builds
// from the same commit can start from different bytes, and nothing records which
// ones a given image was built from. The dated directory is immutable, so a
// rebuild from an old tag reproduces the image that tag was tested against.
func TestBuildScriptDoesNotFetchLatest(t *testing.T) {
	script := readBuildScript(t)

	if loc := latestDirectory.FindString(script); loc != "" {
		t.Errorf("the build script fetches its base image from %q, a mutable pointer;\n"+
			"pin the dated directory so a rebuild reproduces the reviewed bytes", loc)
	}
}

// A download without a digest check is the whole of the supply chain for every
// guest image bladerunner publishes. Debian serves these over HTTPS, which
// authenticates the mirror and nothing about the file: a compromised or
// substituted mirror hands over a qcow2 that the build then bakes, signs and
// ships.
//
// internal/imagebuild/basepins.sha512 already holds reviewed digests, in a
// format sha512sum -c reads directly. The claim this test holds is that the
// shell path and the Go path start from the SAME reviewed bytes — AGENTS.md
// rule 5.7, because a comment asserting it would go wrong in silence the first
// time either side moved.
func TestBuildScriptVerifiesTheBaseAgainstThePins(t *testing.T) {
	script := readBuildScript(t)

	if !strings.Contains(script, basePinsFileName) {
		t.Errorf("the build script never reads %s, so the shell path and the Go path\n"+
			"can start from different images with nothing to notice", basePinsFileName)
	}
	if !strings.Contains(script, "sha512sum") {
		t.Error("the build script never checks a sha512 digest of the downloaded base image")
	}
}

// The pin file is the single source of truth for which Debian build is baked.
// If the script derived the stamp separately — a second constant, an argument,
// a date — the two could disagree while both looked correct, which is the
// four-copies problem this package exists to end. Deriving the filename FROM
// the pin file makes disagreement unrepresentable.
func TestBuildScriptDerivesTheStampFromThePins(t *testing.T) {
	script := readBuildScript(t)

	// The stamp must not be restated as its own literal anywhere in the script.
	if strings.Contains(script, debianStamp) {
		t.Errorf("the build script hardcodes the stamp %q; derive it from %s instead,\n"+
			"or the two drift the first time one is bumped", debianStamp, basePinsFileName)
	}
}

// readBuildScript returns the shell build's source, or skips when it is gone.
func readBuildScript(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(buildScriptPath(t))
	if err != nil {
		t.Fatalf("read build script: %v", err)
	}
	return string(body)
}

// basePinsFileName is used by the shell build and by the tests above, but
// //go:embed needs a string literal and cannot take the constant. That leaves
// two spellings of one filename, which is the drift this package exists to end.
// Reading the file by the constant's name proves they still agree.
func TestBasePinsFileNameMatchesTheEmbed(t *testing.T) {
	onDisk, err := os.ReadFile(basePinsFileName)
	if err != nil {
		t.Fatalf("read %s by its constant: %v", basePinsFileName, err)
	}
	if string(onDisk) != basePinsRaw {
		t.Errorf("%s does not match the embedded copy; the //go:embed directive names a different file", basePinsFileName)
	}
}

// Both architectures the build supports must be pinned. A missing pin makes
// FetchBase fail closed, which is correct, but it fails at build time on one
// architecture only — and the release workflow builds them independently, so
// the other would publish and the set would be half-updated.
func TestBothArchesArePinned(t *testing.T) {
	for _, arch := range []string{"arm64", "amd64"} {
		release, err := BaseRelease(arch)
		if err != nil {
			t.Fatalf("BaseRelease(%q): %v", arch, err)
		}
		digest, err := release.PinnedDigest()
		if err != nil {
			t.Errorf("%s has no reviewed digest: %v", arch, err)
			continue
		}
		if len(digest) != sha512HexLength {
			t.Errorf("%s digest is %d hex characters, want %d", arch, len(digest), sha512HexLength)
		}
	}
}
