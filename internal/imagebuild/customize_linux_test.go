//go:build linux

package imagebuild

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/provision"
)

// baseImageEnv names a qcow2 to customize. The mechanic cannot be exercised
// without a real guest image, and one is too large to keep in the tree, so the
// test is opt-in through the environment.
const baseImageEnv = "BLADERUNNER_TEST_BASE_IMAGE"

// requireNativeMechanic skips unless this machine can actually attach and mount
// a guest image. Every condition is reported by name: a silent skip here would
// make a green run indistinguishable from one that tested nothing.
func requireNativeMechanic(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("short mode: the native mechanic needs a real guest image")
	}
	if os.Geteuid() != 0 {
		t.Skip("not root: attaching and mounting a guest image needs euid 0")
	}
	if _, err := os.Stat(defaultNBDDevice); err != nil {
		t.Skipf("%s is absent: load the nbd module to run this", defaultNBDDevice)
	}
	image := os.Getenv(baseImageEnv)
	if image == "" {
		t.Skipf("%s is unset: point it at a Debian genericcloud qcow2 to run this", baseImageEnv)
	}
	//nolint:gosec // G703: the path is an env var the operator running the test set.
	if _, err := os.Stat(image); err != nil {
		t.Fatalf("%s=%s is not readable: %v", baseImageEnv, image, err)
	}
	return image
}

// copyBaseImage returns a writable copy, because Customize edits in place and
// the source is an expensive download shared with other tests.
func copyBaseImage(t *testing.T, src string) string {
	t.Helper()
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read base image: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "work.qcow2")
	//nolint:gosec // G703: dst is inside this test's own temporary directory.
	if err := os.WriteFile(dst, body, 0o600); err != nil {
		t.Fatalf("copy base image: %v", err)
	}
	return dst
}

// offlineRecipe exercises every step kind without needing a package mirror.
// Network-dependent steps are covered by the full build in CI; what this test
// proves is the mechanic — attach, mount, chroot, apply, detach.
func offlineRecipe() Recipe {
	return Recipe{
		EnableUnits:      nil,
		InitramfsModules: []string{"vhost_vsock"},
		Assets: []provision.GuestAsset{
			{GuestPath: "/etc/bladerunner-test-asset", Mode: 0o644, Content: "asset\n"},
		},
		VersionPath: "/etc/bladerunner-image-version",
		Version:     testVersion,
	}
}

// Steps for the offline recipe, minus the apt scaffolding it cannot run.
func offlineSteps(r Recipe) []Step {
	var out []Step
	for _, s := range r.Steps() {
		if s.Kind == StepRun && strings.Contains(strings.Join(s.Argv, " "), "apt-get") {
			continue
		}
		out = append(out, s)
	}
	// A command that proves the chroot itself works: it must resolve through
	// the guest's PATH and see the guest's filesystem, not the host's.
	return append(out, Step{
		Kind: StepRun,
		Desc: "prove the chroot sees the guest root",
		Argv: []string{"/bin/sh", "-c", "test -f /etc/debian_version && cat /etc/debian_version > /etc/bladerunner-chroot-proof"},
	})
}

// This is the only test that exercises the mechanic end to end against a real
// image: attach over nbd, locate the root partition through the GPT, mount it,
// chroot in, apply the steps, and unwind cleanly.
func TestCustomizeAppliesStepsToARealImage(t *testing.T) {
	base := requireNativeMechanic(t)
	image := copyBaseImage(t, base)

	recipe := offlineRecipe()
	logged := &logCollector{}
	_, err := Customize(t.Context(), Options{
		BaseImage: image,
		WorkDir:   t.TempDir(),
		Steps:     offlineSteps(recipe),
		Log:       logged.add,
	})
	if err != nil {
		t.Fatalf("Customize: %v\nlog:\n%s", err, logged)
	}

	// The device must be free afterwards. A leaked attachment leaves the image
	// unreadable and blocks the next build on the machine.
	if attached(t) {
		t.Errorf("%s is still attached after Customize returned", defaultNBDDevice)
	}

	assertGuestFiles(t, image, map[string]string{
		"/etc/bladerunner-test-asset":    "asset\n",
		"/etc/bladerunner-image-version": testVersion,
	})
}

// A failure part-way through must still detach. Otherwise one bad recipe makes
// every later build on the machine fail for an unrelated reason.
func TestCustomizeDetachesAfterAFailedStep(t *testing.T) {
	base := requireNativeMechanic(t)
	image := copyBaseImage(t, base)

	steps := append(offlineSteps(offlineRecipe()), Step{
		Kind: StepRun,
		Desc: "a step that cannot succeed",
		Argv: []string{"/bin/sh", "-c", "exit 7"},
	})

	_, err := Customize(t.Context(), Options{
		BaseImage: image,
		WorkDir:   t.TempDir(),
		Steps:     steps,
	})
	if err == nil {
		t.Fatal("Customize reported success though a step exited non-zero")
	}
	if attached(t) {
		t.Errorf("%s is still attached after a failed build", defaultNBDDevice)
	}
}

// The build must leave the image's own resolver configuration behind, not the
// build host's. A baked image carrying the builder's resolv.conf resolves DNS
// differently from stock Debian on every guest it ever boots.
func TestCustomizeRestoresTheImageResolver(t *testing.T) {
	base := requireNativeMechanic(t)
	image := copyBaseImage(t, base)

	before := guestResolvLink(t, image)

	recipe := offlineRecipe()
	if _, err := Customize(t.Context(), Options{
		BaseImage: image,
		WorkDir:   t.TempDir(),
		Steps:     offlineSteps(recipe),
	}); err != nil {
		t.Fatalf("Customize: %v", err)
	}

	if after := guestResolvLink(t, image); after != before {
		t.Errorf("resolv.conf is %q after the build, was %q before", after, before)
	}
}

// logCollector gathers progress lines so a failure can show what the build did.
type logCollector struct {
	lines []string
}

func (l *logCollector) add(line string) { l.lines = append(l.lines, line) }
func (l *logCollector) String() string  { return strings.Join(l.lines, "\n") }

// attached reports whether the nbd device still has an image connected. A
// disconnected device reports a size of zero sectors.
func attached(t *testing.T) bool {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(sysBlockDir, filepath.Base(defaultNBDDevice), "size"))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(body)) != "0"
}

// withMountedImage attaches the image read-only-ish and hands the mounted root
// to fn. Verifying through a second attach is the point: it proves the changes
// reached the qcow2 rather than only the page cache of the first mount.
func withMountedImage(t *testing.T, image string, fn func(root string)) {
	t.Helper()
	mount, err := attachImage(t.Context(), image, t.TempDir(), defaultNBDDevice)
	if err != nil {
		t.Fatalf("re-attach %s to inspect it: %v", image, err)
	}
	defer func() {
		if err := mount.Close(); err != nil {
			t.Errorf("detach after inspection: %v", err)
		}
	}()
	fn(mount.Root)
}

// assertGuestFiles checks the contents of files inside the built image.
func assertGuestFiles(t *testing.T, image string, want map[string]string) {
	t.Helper()
	withMountedImage(t, image, func(root string) {
		for guestPath, wantBody := range want {
			rel, err := filepath.Rel("/", guestPath)
			if err != nil {
				t.Fatalf("relative path for %s: %v", guestPath, err)
			}
			got, err := os.ReadFile(filepath.Join(root, rel))
			if err != nil {
				t.Errorf("read %s from the built image: %v", guestPath, err)
				continue
			}
			if string(got) != wantBody {
				t.Errorf("%s = %q, want %q", guestPath, got, wantBody)
			}
		}
	})
}

// guestResolvLink returns the image's resolv.conf as the image stores it: the
// symlink target when it is a symlink, and its contents otherwise.
func guestResolvLink(t *testing.T, image string) string {
	t.Helper()
	var out string
	withMountedImage(t, image, func(root string) {
		target := filepath.Join(root, "etc", "resolv.conf")
		info, err := os.Lstat(target)
		if err != nil {
			out = "<absent>"
			return
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, linkErr := os.Readlink(target)
			if linkErr != nil {
				t.Fatalf("read resolv.conf link: %v", linkErr)
			}
			out = "-> " + link
			return
		}
		body, readErr := os.ReadFile(target)
		if readErr != nil {
			t.Fatalf("read resolv.conf: %v", readErr)
		}
		out = string(body)
	})
	return out
}
