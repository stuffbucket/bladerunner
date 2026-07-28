package util

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestSafeJoinRejectsTraversal drives the containment rule that internal/update
// applies to attacker-controlled tar header names when it unpacks an update
// bundle. The payloads are the already-decoded forms such a name can take: a
// URL-encoded "..%2f" reaches this code as a literal "../".
//
// internal/update carried its own weaker copy of this rule. That copy assumed a
// pre-cleaned base and rejected a legitimate path when the base was not already
// cleaned; the "uncleaned base" case below is the one it failed.
func TestSafeJoinRejectsTraversal(t *testing.T) {
	base := t.TempDir()

	escapes := []struct {
		name    string
		payload string
	}{
		{"parent traversal", "../evil"},
		{"deep traversal", "../../../../etc/passwd"},
		{"traversal mid-path", "Foo.app/../../evil"},
		{"dot-slash then traversal", "./../evil"},
		{"decoded percent-2f traversal", "../../evil"},
		{"sibling with a shared prefix", "../" + filepath.Base(base) + "-evil/x"},
		{"bare parent", ".."},
	}
	for _, tc := range escapes {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			got, err := SafeJoin(base, tc.payload)
			if err == nil {
				t.Fatalf("SafeJoin(%q, %q) = %q, want an escape error", base, tc.payload, got)
			}
			var esc *PathEscapeError
			if !errors.As(err, &esc) {
				t.Fatalf("SafeJoin(%q, %q) error = %v, want a *PathEscapeError", base, tc.payload, err)
			}
		})
	}

	// "..%2f../evil" contains no separator before the "..", so it is a single
	// component literally named "..%2f.." plus "evil". Undecoded it is an
	// ordinary (if odd) file name and must be contained rather than refused;
	// its decoded form "../../evil" is covered above.
	relBase := "." + string(filepath.Separator) + strings.TrimPrefix(base, string(filepath.Separator))

	allowed := []struct {
		name    string
		base    string
		payload string
		want    string
	}{
		{"nested file", base, "Foo.app/Contents/MacOS/br", filepath.Join(base, "Foo.app", "Contents", "MacOS", "br")},
		{"absolute payload is contained, not honored", base, "/etc/passwd", filepath.Join(base, "etc", "passwd")},
		{"base is the destination itself", base, ".", base},
		{"undecoded percent-2f is an ordinary name", base, "..%2f../evil", filepath.Join(base, "..%2f..", "evil")},
		{"uncleaned base with a dot prefix", relBase, "Foo.app/br", filepath.Join(relBase, "Foo.app", "br")},
	}
	for _, tc := range allowed {
		t.Run("allows "+tc.name, func(t *testing.T) {
			got, err := SafeJoin(tc.base, tc.payload)
			if err != nil {
				t.Fatalf("SafeJoin(%q, %q) rejected a legitimate path: %v", tc.base, tc.payload, err)
			}
			if got != tc.want {
				t.Fatalf("SafeJoin(%q, %q) = %q, want %q", tc.base, tc.payload, got, tc.want)
			}
		})
	}
}
