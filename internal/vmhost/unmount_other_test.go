//go:build !darwin

package vmhost

import (
	"testing"

	"github.com/stuffbucket/bladerunner/internal/instance"
)

// Off darwin there is no DiskArbitration, so a cartridge instance runs with no
// unmount veto at all. That must be REPORTED rather than assumed: the whole
// point of the recorded reason is that "unprotected" is never silent, and a
// platform without the framework is the largest instance of it.
func TestStartUnmountWatchReportsTheUnsupportedPlatform(t *testing.T) {
	host, err := New(Spec{
		Kind:          instance.KindCartridge,
		Name:          "veto-test",
		CartridgePath: "/tmp/demo.dmg",
		Mountpoint:    "/Volumes/bladerunner-demo",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := host.startUnmountWatch(); err != nil {
		t.Fatalf("startUnmountWatch failed the start: %v — the veto must fail open", err)
	}
	if got := host.UnmountProtection(); got != UnprotectedUnsupported {
		t.Fatalf("UnmountProtection() = %q, want %q", got, UnprotectedUnsupported)
	}
	if host.unmountCancel != nil {
		t.Fatal("no watcher can exist without DiskArbitration")
	}
}

// An instance with no cartridge has nothing to protect anywhere, so it must not
// report itself unprotected even on a platform that could never protect it —
// nor claim UnprotectedNone, which means the veto was armed.
func TestStartUnmountWatchIsSilentForFlatInstancesOffDarwin(t *testing.T) {
	host := newTestHost(t)
	if err := host.startUnmountWatch(); err != nil {
		t.Fatalf("startUnmountWatch: %v", err)
	}
	if got := host.UnmountProtection(); got != UnprotectedNotRecorded {
		t.Fatalf("UnmountProtection() for a flat instance = %q, want %q", got, UnprotectedNotRecorded)
	}
}
