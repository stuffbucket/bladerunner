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
