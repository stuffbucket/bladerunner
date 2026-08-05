package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tests in this file hold the containment rule of updater extraction: no
// archive entry may put a byte outside the single top-level .app bundle, whether
// it tries with a "../" name, with a symlink whose target resolves out of the
// tree, or by writing through a symlink component that already exists on disk.

const appDirName = "Bladerunner.app"

// escapingLink is a symlink target that resolves outside the extraction root
// while its text starts with a normal component, so a check that only screens a
// leading ".." lets it through. Read from Bladerunner.app/Contents it walks into
// the existing MacOS directory and then four levels up, which is one level above
// the destination.
const escapingLink = "MacOS/../../../../victim"

// extractSandbox returns a destination directory plus a sibling "victim"
// directory outside it. An escape attempt that succeeds lands in victim, so the
// test can prove nothing was written there.
func extractSandbox(t *testing.T) (dest, victim string) {
	t.Helper()
	root := t.TempDir()
	dest = filepath.Join(root, "dest")
	victim = filepath.Join(root, "victim")
	for _, d := range []string{dest, victim} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dest, victim
}

// assertEmpty fails if dir has any entry, naming what escaped.
func assertEmpty(t *testing.T, dir, what string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	if len(entries) != 0 {
		got := make([]string, 0, len(entries))
		for _, e := range entries {
			got = append(got, e.Name())
		}
		t.Fatalf("%s: %d entries escaped into %s: %v", what, len(got), dir, got)
	}
}

// bundleSkeleton is the pair of directory entries every escape case starts from.
func bundleSkeleton() []tarEntry {
	return []tarEntry{
		{name: appDirName + "/Contents/", dir: true},
		{name: appDirName + "/Contents/MacOS/", dir: true},
	}
}

// TestExtractAppBundle_RejectsWriteThroughSymlink is the write-through case: one
// entry creates a symlink whose target text passes a literal "../" screen, and a
// later entry writes a regular file through that symlink. The bytes land outside
// the destination unless the extractor refuses to traverse a symlink component.
func TestExtractAppBundle_RejectsWriteThroughSymlink(t *testing.T) {
	dest, victim := extractSandbox(t)

	tarball := buildRawTarball(t, append(bundleSkeleton(),
		tarEntry{name: appDirName + "/Contents/link", linkname: escapingLink},
		tarEntry{name: appDirName + "/Contents/link/pwned.sh", body: "pwned"},
	))

	if _, err := extractAppBundle(tarball, dest); err == nil {
		t.Fatal("expected extract to reject a regular file written through a symlink")
	}
	assertEmpty(t, victim, "write-through symlink")
}

// TestExtractAppBundle_RejectsChainedSymlink is the same escape with a symlink
// as the second entry, so the link itself is created outside the destination.
func TestExtractAppBundle_RejectsChainedSymlink(t *testing.T) {
	dest, victim := extractSandbox(t)

	tarball := buildRawTarball(t, append(bundleSkeleton(),
		tarEntry{name: appDirName + "/Contents/link", linkname: escapingLink},
		tarEntry{name: appDirName + "/Contents/link/out", linkname: "elsewhere"},
	))

	if _, err := extractAppBundle(tarball, dest); err == nil {
		t.Fatal("expected extract to reject a symlink created through a symlink")
	}
	assertEmpty(t, victim, "chained symlink")
}

// TestExtractAppBundle_RejectsPreexistingSymlinkComponent holds the no-follow
// rule on its own: the destination already carries a symlink where the bundle
// goes, and an ordinary archive must not be written through it.
func TestExtractAppBundle_RejectsPreexistingSymlinkComponent(t *testing.T) {
	dest, victim := extractSandbox(t)
	if err := os.Symlink(victim, filepath.Join(dest, appDirName)); err != nil {
		t.Fatal(err)
	}

	tarball := buildAppTarball(t, map[string]string{"Contents/MacOS/br": "#!/bin/sh\n"})
	if _, err := extractAppBundle(tarball, dest); err == nil {
		t.Fatal("expected extract to reject a symlink already standing in the destination")
	}
	assertEmpty(t, victim, "preexisting symlink component")
}

// TestExtractAppBundle_RejectsEscapingSymlinkTargets covers symlink targets that
// resolve outside the bundle without their text starting with "..".
func TestExtractAppBundle_RejectsEscapingSymlinkTargets(t *testing.T) {
	cases := []struct {
		name     string
		linkname string
	}{
		{"nested dotdot", "safe/../../../escape"},
		{"deeply nested dotdot", "a/b/c/../../../../../../escape"},
		{"self relative escape", "./../../escape"},
		{"resolvable escape", escapingLink},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dest, victim := extractSandbox(t)
			tarball := buildRawTarball(t, append(bundleSkeleton(),
				tarEntry{name: appDirName + "/Contents/link", linkname: tc.linkname},
			))
			if _, err := extractAppBundle(tarball, dest); err == nil {
				t.Fatalf("expected extract to reject symlink target %q", tc.linkname)
			}
			assertEmpty(t, victim, "escaping symlink target")
		})
	}
}

// TestExtractAppBundle_RejectsEntryOutsideBundle holds that an entry inside the
// destination but outside the selected .app is refused rather than merely staged.
func TestExtractAppBundle_RejectsEntryOutsideBundle(t *testing.T) {
	dest, victim := extractSandbox(t)
	tarball := buildRawTarball(t, []tarEntry{
		{name: appDirName + "/Contents/MacOS/br", body: "ok"},
		{name: "sidecar/payload.sh", body: "payload"},
	})
	if _, err := extractAppBundle(tarball, dest); err == nil {
		t.Fatal("expected extract to reject an entry outside the .app bundle")
	}
	if _, err := os.Stat(filepath.Join(dest, "sidecar")); !os.IsNotExist(err) {
		t.Fatalf("entry outside the bundle was staged anyway: stat err=%v", err)
	}
	assertEmpty(t, victim, "entry outside bundle")
}

// TestExtractAppBundle_RejectsSecondBundle holds that only one top-level .app is
// accepted, so a second bundle cannot ride along beside the one we install.
func TestExtractAppBundle_RejectsSecondBundle(t *testing.T) {
	dest, _ := extractSandbox(t)
	tarball := buildRawTarball(t, []tarEntry{
		{name: appDirName + "/Contents/MacOS/br", body: "ok"},
		{name: "Other.app/Contents/MacOS/br", body: "other"},
	})
	if _, err := extractAppBundle(tarball, dest); err == nil {
		t.Fatal("expected extract to reject a second top-level .app")
	}
	if _, err := os.Stat(filepath.Join(dest, "Other.app")); !os.IsNotExist(err) {
		t.Fatalf("second bundle was staged anyway: stat err=%v", err)
	}
}

// TestExtractAppBundle_AcceptsInBundleSymlink holds that the rule does not cost
// legitimate bundles anything: framework-style relative symlinks that resolve
// inside the .app are created as written.
func TestExtractAppBundle_AcceptsInBundleSymlink(t *testing.T) {
	dest, _ := extractSandbox(t)
	const fw = appDirName + "/Contents/Frameworks/Br.framework"
	tarball := buildRawTarball(t, []tarEntry{
		{name: appDirName + "/Contents/", dir: true},
		{name: fw + "/Versions/A/", dir: true},
		{name: fw + "/Versions/A/Br", body: "lib"},
		{name: fw + "/Versions/Current", linkname: "A"},
		{name: fw + "/Br", linkname: "Versions/Current/Br"},
		{name: appDirName + "/Contents/MacOS/br", body: "#!/bin/sh\n"},
	})
	appRoot, err := extractAppBundle(tarball, dest)
	if err != nil {
		t.Fatalf("legitimate in-bundle symlinks were rejected: %v", err)
	}
	link := filepath.Join(appRoot, "Contents", "Frameworks", "Br.framework", "Br")
	got, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink %s: %v", link, err)
	}
	if got != "Versions/Current/Br" {
		t.Fatalf("link target = %q", got)
	}
	// The link resolves to real content through the Current hop.
	body, err := os.ReadFile(link)
	if err != nil {
		t.Fatalf("read through link: %v", err)
	}
	if string(body) != "lib" {
		t.Fatalf("content through link = %q", body)
	}
}

// TestExtractAppBundle_IgnoresNonFileEntries holds that headers which create
// nothing — a pax global header, a fifo — do not fail the one-bundle rule. Real
// archivers emit them beside the bundle, and refusing them would break updates
// that carry no escape at all.
func TestExtractAppBundle_IgnoresNonFileEntries(t *testing.T) {
	dest, _ := extractSandbox(t)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	headers := []*tar.Header{
		{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "x"}},
		{Name: appDirName + "/Contents/MacOS/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "dev/null", Typeflag: tar.TypeFifo, Mode: 0o644},
	}
	for _, h := range headers {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: appDirName + "/Contents/MacOS/br", Typeflag: tar.TypeReg, Mode: 0o755, Size: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	appRoot, err := extractAppBundle(buf.Bytes(), dest)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if _, err := os.Stat(filepath.Join(appRoot, "Contents", "MacOS", "br")); err != nil {
		t.Fatalf("bundle file missing: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "dev")); !os.IsNotExist(err) {
		t.Fatalf("fifo entry created something: stat err=%v", err)
	}
}

// TestExtractAppBundle_RejectsAbsoluteSymlink keeps the absolute-target refusal
// covered and checks the message names what was refused.
func TestExtractAppBundle_RejectsAbsoluteSymlink(t *testing.T) {
	dest, victim := extractSandbox(t)
	tarball := buildRawTarball(t, append(bundleSkeleton(),
		tarEntry{name: appDirName + "/Contents/link", linkname: filepath.Join(victim, "target")},
	))
	_, err := extractAppBundle(tarball, dest)
	if err == nil {
		t.Fatal("expected extract to reject an absolute symlink target")
	}
	if !strings.Contains(err.Error(), "Contents/link") {
		t.Fatalf("error does not name the refused entry: %v", err)
	}
	assertEmpty(t, victim, "absolute symlink")
}
