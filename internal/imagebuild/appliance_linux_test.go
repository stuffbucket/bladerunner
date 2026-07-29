//go:build linux

package imagebuild

import (
	"context"
	"os/exec"
	"testing"
)

// This is the only test that boots a real libguestfs appliance, so it is gated
// behind -short like the project's other hardware tests. It exists because the
// probe's whole purpose is distinguishing a working libguestfs from a merely
// installed one, and every other test only ever observes the false result:
// without this, applianceUsable could return false unconditionally and the
// suite would stay green while every fallback silently became unavailable.
//
// It also exercises the two settings applianceEnv applies. On aarch64 without
// KVM the appliance does not boot at all unless force_tcg is in effect, so a
// pass here is direct evidence that the diagnosed workaround still works
// against the installed libguestfs, not merely that the string is present.
func TestApplianceUsableAgainstRealLibguestfs(t *testing.T) {
	if testing.Short() {
		t.Skip("boots a libguestfs appliance; skipped under -short")
	}
	if _, err := exec.LookPath(applianceProbeTool); err != nil {
		t.Skipf("%s not installed; nothing to probe", applianceProbeTool)
	}

	if !applianceUsable(context.Background()) {
		t.Error("applianceUsable() = false with libguestfs installed; " +
			"the appliance failed to launch (check LIBGUESTFS_BACKEND_SETTINGS=force_tcg still applies)")
	}
}

// A probe that ignores its deadline would stall a build behind a wedged
// appliance, so the timeout path is asserted separately from the success path.
func TestApplianceUsableGivesUpOnACanceledContext(t *testing.T) {
	if _, err := exec.LookPath(applianceProbeTool); err != nil {
		t.Skipf("%s not installed", applianceProbeTool)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if applianceUsable(ctx) {
		t.Error("applianceUsable() = true with a canceled context, want false")
	}
}
