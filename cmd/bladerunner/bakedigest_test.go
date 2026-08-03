package main

import (
	"strings"
	"testing"
)

// The build reports its digest on stdout, and that is the only place the digest
// may come from.
//
// disk.go used to fall back to reading <output>.sha256 when stdout was empty.
// That fallback could not fire on a successful build — the script runs under
// `set -euo pipefail` and prints the digest unconditionally after computing it,
// so exit 0 implies a digest on stdout — but if it ever did, it would read a
// sidecar with no evidence that THIS build produced it. A leftover from an
// earlier bake at the same path parses as valid and gets stamped into the
// manifest, pairing a fresh image with a stale digest.
//
// The build is already atomic: the image is assembled in the work directory and
// renamed into place as the last step, so either the output and its sidecar are
// both fresh or the output was never written. There is no partial state for a
// fallback to recover, which is why the correction is to delete it rather than
// to guard it.
func TestBuildDigestComesOnlyFromStdout(t *testing.T) {
	const valid = "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"

	for _, tc := range []struct {
		name    string
		stdout  string
		want    string
		wantErr string
	}{
		{"a bare digest", valid + "\n", valid, ""},
		{"surrounding whitespace", "  " + valid + "  \n", valid, ""},
		{"empty output", "", "", "no digest"},
		{"whitespace only", "   \n", "", "no digest"},
		{"not a digest", "something went wrong\n", "", "not a valid sha256"},
		{"truncated digest", valid[:40] + "\n", "", "not a valid sha256"},
		{"uppercase digest", strings.ToUpper(valid) + "\n", "", "not a valid sha256"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildDigest([]byte(tc.stdout))

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("buildDigest(%q): %v", tc.stdout, err)
				}
				if got != tc.want {
					t.Errorf("digest = %q, want %q", got, tc.want)
				}
				return
			}

			if err == nil {
				t.Fatalf("buildDigest(%q) returned %q, want an error", tc.stdout, got)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// The build's stdout is NOT just the digest, whatever disk.go's comment used to
// say. The nbd mechanic runs apt inside a chroot, and neither apt-get update nor
// apt-get install has its stdout redirected (scripts/build-guest-image.sh), so
// every package line lands in the same stream this parses. Only the script's own
// logging goes to stderr, via log() at :71.
//
// Treating the whole stream as the digest therefore fails a build that
// succeeded: the image is complete and renamed into place, and the bake then
// reports "not a valid sha256" and stamps nothing. The digest is the last line
// the script prints, so that is what this reads.
//
// CI never hit this because the release workflow invokes the script directly
// rather than through `br disk bake`. It is reachable only by a user running the
// documented command, which is the worst place to leave it.
func TestBuildDigestIgnoresPrecedingBuildOutput(t *testing.T) {
	const valid = "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"

	noisy := strings.Join([]string{
		"Get:1 http://deb.debian.org/debian trixie InRelease [138 kB]",
		"Reading package lists...",
		"Setting up incus (6.0.4-2+deb13u9) ...",
		valid,
		"",
	}, "\n")

	got, err := buildDigest([]byte(noisy))
	if err != nil {
		t.Fatalf("buildDigest rejected a successful build's output: %v", err)
	}
	if got != valid {
		t.Errorf("digest = %q, want %q", got, valid)
	}
}

// A build whose last line is not a digest must still fail. Reading the last line
// rather than the whole stream must not become "find a digest anywhere", or a
// digest printed mid-build by some future step would be preferred over the real
// one the script emits last.
func TestBuildDigestStillRejectsATrailingNonDigest(t *testing.T) {
	const valid = "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"

	trailing := valid + "\nE: Sub-process returned an error code\n"
	if got, err := buildDigest([]byte(trailing)); err == nil {
		t.Errorf("buildDigest returned %q for output ending in an error line, want an error", got)
	}
}
