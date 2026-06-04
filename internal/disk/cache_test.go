package disk

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/util"
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

// TestCacheHitMiss exercises the content-addressed hit/miss logic without
// requiring qemu-img: a cache "hit" is a present <sha>.raw guarded by a
// sibling ".ok" stamp; absence of either is a miss. This mirrors the gate the
// vm materialize path uses (sha256-addressed raw + ".ok" stamp).
func TestCacheHitMiss(t *testing.T) {
	state := t.TempDir()
	t.Setenv("BLADERUNNER_STATE_DIR", state)

	sha := strings.Repeat("cd", 32)
	cp := CachePath(sha)

	// Miss: nothing present.
	if cacheHit(cp) {
		t.Fatal("expected miss when cache file absent")
	}

	if err := os.MkdirAll(ImageCacheDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cp, []byte("raw-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Still a miss: raw present but unstamped (download/convert never completed).
	if cacheHit(cp) {
		t.Fatal("expected miss when .ok stamp absent")
	}

	if err := os.WriteFile(cp+".ok", nil, 0o644); err != nil {
		t.Fatal(err)
	}

	// Hit: raw + stamp both present.
	if !cacheHit(cp) {
		t.Fatal("expected hit when raw and .ok stamp present")
	}
}

// cacheHit reports whether a content-addressed cache entry is usable: the raw
// image and its ".ok" completion stamp are both present.
func cacheHit(cachePath string) bool {
	return util.FileExists(cachePath) && util.FileExists(cachePath+".ok")
}
