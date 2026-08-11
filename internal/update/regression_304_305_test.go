package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// macOS /usr/bin/tar emits AppleDouble sidecar members by default, even for a
// directory with no manually-set xattrs. `tar -tzf` hides them; Go's
// archive/tar reader does not. The sidecar for a bundle sorts BEFORE the bundle
// directory itself, so whichever entry the extractor sees first defines the
// bundle root -- and "._Bladerunner.app" is not a bundle, it is a 163-byte blob.
//
// Regression test for #304.
func appleDoubleTarball(t *testing.T, bundle string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	write := func(name string, mode int64, typeflag byte, body string) {
		t.Helper()
		hdr := &tar.Header{Name: name, Mode: mode, Typeflag: typeflag, Size: int64(len(body))}
		if typeflag == tar.TypeDir {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %q: %v", name, err)
		}
		if typeflag != tar.TypeDir && body != "" {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatalf("write body %q: %v", name, err)
			}
		}
	}

	// Exactly the ordering real macOS tar produces.
	write("._"+bundle, 0o644, tar.TypeReg, strings.Repeat("A", 163))
	write(bundle+"/", 0o755, tar.TypeDir, "")
	write(bundle+"/._Contents", 0o644, tar.TypeReg, strings.Repeat("B", 163))
	write(bundle+"/Contents/", 0o755, tar.TypeDir, "")
	write(bundle+"/Contents/Info.plist", 0o644, tar.TypeReg, "<plist/>")
	write(bundle+"/Contents/MacOS/", 0o755, tar.TypeDir, "")
	write(bundle+"/Contents/MacOS/run", 0o755, tar.TypeReg, "#!/bin/sh\n")

	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

func TestExtractAppBundle_AcceptsMacOSTarWithAppleDoubleSidecars(t *testing.T) {
	dest := t.TempDir()

	appRoot, err := extractAppBundle(appleDoubleTarball(t, "Bladerunner.app"), dest)
	if err != nil {
		t.Fatalf("extractAppBundle rejected a macOS-tar archive: %v", err)
	}

	if got, want := filepath.Base(appRoot), "Bladerunner.app"; got != want {
		t.Errorf("appRoot = %q, want the real bundle %q (an AppleDouble sidecar must never define the root)", got, want)
	}

	// The real payload must be present...
	if _, err := os.Stat(filepath.Join(appRoot, "Contents", "Info.plist")); err != nil {
		t.Errorf("real bundle content missing after extraction: %v", err)
	}
	// ...and the sidecars must not have been written anywhere.
	if _, err := os.Stat(filepath.Join(dest, "._Bladerunner.app")); err == nil {
		t.Error("AppleDouble sidecar ._Bladerunner.app was extracted; it must be skipped")
	}
	if _, err := os.Stat(filepath.Join(appRoot, "._Contents")); err == nil {
		t.Error("AppleDouble sidecar ._Contents was extracted; it must be skipped")
	}
}

func TestTopAppComponent_IgnoresAppleDoubleSidecar(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"real bundle dir", "Bladerunner.app/Contents/Info.plist", "Bladerunner.app"},
		{"real bundle root", "Bladerunner.app/", "Bladerunner.app"},
		{"dot-slash prefixed", "./Bladerunner.app/Contents", "Bladerunner.app"},
		{"appledouble sidecar", "._Bladerunner.app", ""},
		{"appledouble with path", "._Bladerunner.app/whatever", ""},
		{"not an app", "notes.txt", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := topAppComponent(tc.in); got != tc.want {
				t.Errorf("topAppComponent(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Two concurrent swapBundle calls must never destroy the installed bundle.
//
// The reported deletion interleaving: A renames dst aside; B's recovery sees a
// backup with dst missing and restores it; A's rename of the new bundle then
// fails ENOTEMPTY; A's deferred restore RemoveAll's dst -- which is now B's
// restored bundle -- and its own backup is gone. Net result: no bundle at all.
//
// Regression test for #305.
func TestSwapBundle_ConcurrentSwapsNeverDestroyTheBundle(t *testing.T) {
	const rounds = 200

	for round := range rounds {
		parent := t.TempDir()
		dst := filepath.Join(parent, "Bladerunner.app")

		mkBundle := func(dir, marker string) {
			t.Helper()
			if err := os.MkdirAll(filepath.Join(dir, "Contents"), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(filepath.Join(dir, "Contents", "marker"), []byte(marker), 0o644); err != nil {
				t.Fatalf("write marker: %v", err)
			}
		}

		mkBundle(dst, "installed")
		newA := filepath.Join(parent, "stage-a.app")
		newB := filepath.Join(parent, "stage-b.app")
		mkBundle(newA, "A")
		mkBundle(newB, "B")

		var wg sync.WaitGroup
		wg.Add(2)
		for _, stage := range []string{newA, newB} {
			go func() {
				defer wg.Done()
				_ = swapBundle(dst, stage)
			}()
		}
		wg.Wait()

		// The invariant: whatever happened, a complete bundle is installed.
		marker, err := os.ReadFile(filepath.Join(dst, "Contents", "marker"))
		if err != nil {
			t.Fatalf("round %d: no bundle survives at %s: %v", round, dst, err)
		}
		switch string(marker) {
		case "A", "B":
			// One of the two updates won. Correct.
		case "installed":
			t.Fatalf("round %d: neither update landed; the app is still on the old version", round)
		default:
			t.Fatalf("round %d: unexpected marker %q", round, marker)
		}
	}
}
