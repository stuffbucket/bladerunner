package imagebuild

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeRoot builds a guest root with the things Describe reads, so the extractor
// can be exercised without a built image or root privileges.
func fakeRoot(t *testing.T, packages, units, modules []string) string {
	t.Helper()
	root := t.TempDir()

	var status strings.Builder
	for _, p := range packages {
		status.WriteString("Package: " + p + "\nStatus: install ok installed\n\n")
	}
	writeUnder(t, root, dpkgStatusPath, status.String())

	for _, u := range units {
		writeUnder(t, root, filepath.Join(enabledUnitsDir, u), "")
	}
	writeUnder(t, root, initramfsModulesPath, strings.Join(modules, "\n")+"\n")
	return root
}

// writeUnder creates a file at a guest-absolute path inside root.
func writeUnder(t *testing.T, root, guestPath, body string) {
	t.Helper()
	full := filepath.Join(root, guestPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", guestPath, err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", guestPath, err)
	}
}

// Describe reads what an image CONTAINS, so two images built by different
// mechanics can be compared on their contents rather than on a list of
// properties someone remembered to check.
//
// That distinction is the whole point. A checklist gate — same packages, same
// UI, same services — polices the differences that already happened and stays
// silent on the next one, which is the defect this repo keeps rediscovering. A
// derived description fails on any difference, including one nobody predicted.
func TestDescribeReadsWhatTheImageContains(t *testing.T) {
	root := fakeRoot(t,
		[]string{"chrony", "incus", "socat"},
		[]string{"incus.service", "ssh.service"},
		[]string{"vhost_vsock", "vmw_vsock_virtio_transport"},
	)
	recipe := DefaultRecipe(testVersion)
	writeUnder(t, root, recipe.VersionPath, testVersion)

	got, err := Describe(root, recipe)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}

	if !slices.Equal(got.Packages, []string{"chrony", "incus", "socat"}) {
		t.Errorf("packages = %v", got.Packages)
	}
	if !slices.Equal(got.Units, []string{"incus.service", "ssh.service"}) {
		t.Errorf("units = %v", got.Units)
	}
	if !slices.Equal(got.InitramfsModules, []string{"vhost_vsock", "vmw_vsock_virtio_transport"}) {
		t.Errorf("modules = %v", got.InitramfsModules)
	}
	if got.Files[recipe.VersionPath] == "" {
		t.Errorf("no digest for %s; the recipe writes it, so it must be compared", recipe.VersionPath)
	}
}

// The set of files compared is DERIVED from the recipe, not listed in the
// extractor. A step added to the recipe joins the comparison with no change
// here — which is the property that makes this gate hold for the next
// divergence rather than the last one.
func TestDescribeComparesEveryPathTheRecipeWrites(t *testing.T) {
	recipe := DefaultRecipe(testVersion)

	var written []string
	for _, s := range recipe.Steps() {
		if s.Kind == StepWriteFile || s.Kind == StepAppendFile {
			written = append(written, s.Path)
		}
	}
	if len(written) == 0 {
		t.Fatal("the recipe writes no files; the extraction is reading the wrong thing")
	}

	root := fakeRoot(t, nil, nil, nil)
	for _, p := range written {
		writeUnder(t, root, p, "contents of "+p)
	}

	got, err := Describe(root, recipe)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	for _, p := range written {
		if got.Files[p] == "" {
			t.Errorf("the recipe writes %s but Describe did not digest it", p)
		}
	}
}

// A file the recipe writes that is ABSENT must be recorded as absent rather
// than skipped. The web-UI divergence is exactly this shape: one mechanic wrote
// a drop-in and the other did not, and a comparison that ignores missing paths
// would call those two images identical.
func TestDescribeRecordsAMissingRecipeFile(t *testing.T) {
	recipe := DefaultRecipe(testVersion)
	root := fakeRoot(t, nil, nil, nil)

	got, err := Describe(root, recipe)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if got.Files[recipe.VersionPath] != absentDigest {
		t.Errorf("a missing recipe file is %q, want %q so a comparison can see it",
			got.Files[recipe.VersionPath], absentDigest)
	}
}

// Two descriptions differing in ONE file must not compare equal. This is the
// assertion the gate rests on.
func TestDescriptionsDifferOnContent(t *testing.T) {
	recipe := DefaultRecipe(testVersion)

	a := fakeRoot(t, nil, nil, nil)
	writeUnder(t, a, recipe.VersionPath, "one")
	b := fakeRoot(t, nil, nil, nil)
	writeUnder(t, b, recipe.VersionPath, "another")

	da, err := Describe(a, recipe)
	if err != nil {
		t.Fatalf("Describe(a): %v", err)
	}
	db, err := Describe(b, recipe)
	if err != nil {
		t.Fatalf("Describe(b): %v", err)
	}
	if diff := da.Diff(db); len(diff) == 0 {
		t.Error("two images whose contents differ compared equal")
	}
}
