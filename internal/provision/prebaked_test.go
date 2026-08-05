package provision

import (
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/config"
)

// The bootstrap must not reinstall what the pre-baked image already has.
//
// That image ships incus, incus-client, socat, jq, openssh-server and chrony,
// and carries a version stamp saying so. The bootstrap re-derived that
// conclusion anyway, at a measured 20s of apt-update against the Debian mirror
// plus 5s of install no-ops on every first boot.
func TestBootstrapSkipsBasePackagesOnAPreBakedImage(t *testing.T) {
	script := renderBootstrapScript(&config.Config{})

	if !strings.Contains(script, config.GuestImageVersionPath) {
		t.Fatalf("the bootstrap never checks %s, so it cannot tell a pre-baked image "+
			"from a bare Debian one", config.GuestImageVersionPath)
	}
	if !strings.Contains(script, "prebaked-skip-base") {
		t.Error("no stage marks the skip, so a boot that took the fast path is " +
			"indistinguishable from one that did not")
	}
}

// The path is written literally in the bootstrap template, because a format
// placeholder there would renumber every positional argument after it. Two
// spellings of one filename is a drift risk, so this proves they agree.
func TestPreBakedStampPathMatchesTheConstant(t *testing.T) {
	script := renderBootstrapScript(&config.Config{})

	if !strings.Contains(script, "[ -f "+config.GuestImageVersionPath+" ]") {
		t.Errorf("the bootstrap does not test for %q; the literal in the template "+
			"has drifted from the constant", config.GuestImageVersionPath)
	}
}

// The Debian fallback must still install everything.
//
// A genericcloud image carries no stamp and genuinely has none of these
// packages. Skipping there would strand a guest with no sshd and no socat,
// which is the control path — the failure this bootstrap is ordered to avoid.
func TestBootstrapStillInstallsWhenThereIsNoStamp(t *testing.T) {
	script := renderBootstrapScript(&config.Config{})

	for _, want := range []string{"apt-install-base", "openssh-server socat jq chrony", "apt_update_retry"} {
		if !strings.Contains(script, want) {
			t.Errorf("the bootstrap no longer contains %q; the Debian fallback needs it", want)
		}
	}
	// The install must be the ELSE of the stamp check, not deleted outright.
	if !strings.Contains(script, "elif command -v apt-get") {
		t.Error("the apt install is no longer guarded as the fallback branch of the stamp check")
	}
}
