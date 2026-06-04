package disk

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestImageCacheDir(t *testing.T) {
	state := t.TempDir()
	t.Setenv("BLADERUNNER_STATE_DIR", state)

	want := filepath.Join(state, "cache", "images")
	if got := ImageCacheDir(); got != want {
		t.Fatalf("ImageCacheDir() = %q, want %q", got, want)
	}

	sha := strings.Repeat("ab", 32) // 64 hex chars
	cp := CachePath(sha)
	if filepath.Dir(cp) != want {
		t.Fatalf("CachePath dir = %q, want %q", filepath.Dir(cp), want)
	}
	if !strings.HasSuffix(cp, sha+".raw") {
		t.Fatalf("CachePath = %q, want suffix %q", cp, sha+".raw")
	}
}
