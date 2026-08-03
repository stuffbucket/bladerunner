package imagebuild

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// buildScriptPath locates the shell build script from this package's directory.
// The script is on its way out — once the Go mechanic lands and parity is
// proven it is deleted — so an absent script skips rather than fails.
func buildScriptPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "scripts", "build-guest-image.sh")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("build script not present at %s; nothing to compare against", path)
	}
	return path
}

// guestPackages matches the script's single declaration of the guest package
// set. There used to be two lists — one per mechanic — and this test needed a
// regex for each shape plus a counter to prove neither had silently stopped
// matching. One declaration removes the drift it was policing and most of the
// machinery that policed it.
var guestPackages = regexp.MustCompile(`GUEST_PACKAGES="([^"]+)"`)

// The recipe declares the package set; until the script is deleted it restates
// it, and the two must agree or the image depends on which mechanic ran.
//
// The script must also declare it ONCE. Two copies drifted in exactly the way
// this package exists to end: the two mechanics could install different sets
// while both looked correct.
func TestRecipePackagesMatchTheBuildScript(t *testing.T) {
	body, err := os.ReadFile(buildScriptPath(t))
	if err != nil {
		t.Fatalf("read build script: %v", err)
	}
	script := string(body)

	declarations := guestPackages.FindAllStringSubmatch(script, -1)
	if len(declarations) != 1 {
		t.Fatalf("the script declares GUEST_PACKAGES %d times, want exactly 1; "+
			"a second copy is a set the other mechanic can drift from", len(declarations))
	}

	want := slices.Clone(DefaultRecipe(testVersion).Packages)
	slices.Sort(want)
	if got := splitSorted(declarations[0][1], " "); !slices.Equal(got, want) {
		t.Errorf("script packages = %v, recipe = %v", got, want)
	}

	// Both mechanics must consume that declaration rather than restate it.
	for _, use := range []string{
		`--install "${GUEST_PACKAGES_CSV}"`,
		`apt-get install -y ${GUEST_PACKAGES}`,
	} {
		if !strings.Contains(script, use) {
			t.Errorf("no mechanic uses %s; a restated list can drift from the declaration", use)
		}
	}
}

// The initramfs modules are the other half of the recipe the script restates.
func TestRecipeInitramfsModulesMatchTheBuildScript(t *testing.T) {
	body, err := os.ReadFile(buildScriptPath(t))
	if err != nil {
		t.Fatalf("read build script: %v", err)
	}
	script := string(body)

	for _, mod := range DefaultRecipe("2026.07.29").InitramfsModules {
		if !strings.Contains(script, mod) {
			t.Errorf("build script does not mention initramfs module %q from the recipe", mod)
		}
	}
}

// systemctlEnable matches an enable invocation and captures its unit list. The
// script enables units both singly and in groups, so the list is variadic.
var systemctlEnable = regexp.MustCompile(`systemctl enable ([a-zA-Z0-9@._ -]+)`)

// A unit enabled by one mechanic and not the other is the worst kind of drift:
// both builds succeed, both images boot, and only one of them actually starts
// the service.
//
// The script's set is extracted rather than searched for, so a unit added to the
// recipe and forgotten in the script fails here. A membership test would instead
// pass as soon as any grouped enable line existed, which is how this assertion
// would quietly stop testing anything.
func TestRecipeEnabledUnitsMatchTheBuildScript(t *testing.T) {
	body, err := os.ReadFile(buildScriptPath(t))
	if err != nil {
		t.Fatalf("read build script: %v", err)
	}

	inScript := map[string]bool{}
	for _, m := range systemctlEnable.FindAllStringSubmatch(string(body), -1) {
		for _, unit := range strings.Fields(m[1]) {
			inScript[unit] = true
		}
	}
	if len(inScript) == 0 {
		t.Fatal("no systemctl enable invocations found; the extraction pattern has drifted from the script")
	}

	for _, unit := range DefaultRecipe("2026.07.29").EnableUnits {
		// The script writes bare names where the recipe is explicit about the
		// unit type, and systemd treats the two as the same unit.
		if inScript[unit] || inScript[strings.TrimSuffix(unit, ".service")] {
			continue
		}
		t.Errorf("build script never enables unit %q from the recipe (script enables %v)", unit, sortedKeys(inScript))
	}
}

// sortedKeys renders a set deterministically, so a failure message does not
// change between runs.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// The version stamp path is how a booted guest reports its image. The script
// writes it as a literal, so a change to the config constant must reach it.
func TestRecipeVersionPathMatchesTheBuildScript(t *testing.T) {
	body, err := os.ReadFile(buildScriptPath(t))
	if err != nil {
		t.Fatalf("read build script: %v", err)
	}

	path := DefaultRecipe("2026.07.29").VersionPath
	if !strings.Contains(string(body), path) {
		t.Errorf("build script does not write the version stamp to %q", path)
	}
}

// splitSorted splits on sep, trims, drops empties, and sorts, so two package
// lists written in different orders compare equal.
func splitSorted(s, sep string) []string {
	parts := strings.Split(s, sep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}
