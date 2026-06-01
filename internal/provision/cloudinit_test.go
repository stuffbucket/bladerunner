package provision

import (
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/config"
)

// testConfig returns a minimal but valid *config.Config sufficient to drive
// BuildCloudInit. Only the fields the cloud-init renderer dereferences are
// populated; arch defaults to arm64 to match the primary Apple-silicon target.
func testConfig() *config.Config {
	return &config.Config{
		Name:          "test",
		Hostname:      "bladerunner-test",
		SSHUser:       "tester",
		SSHPublicKey:  "ssh-ed25519 AAAA test@bladerunner",
		Arch:          "arm64",
		OIDCIssuerURL: "http://127.0.0.1:15556",
		OIDCClientID:  "bladerunner",
		OIDCAudience:  "bladerunner",
		LocalOIDCPort: 15556,
		VsockSSHPort:  10022,
		VsockAPIPort:  18443,
		VsockOIDCPort: 18556,
	}
}

// TestBuildCloudInit_FirstBootConsoleDropIn verifies that the user-data
// emitted by BuildCloudInit installs the /etc/default/grub.d/99_bladerunner.cfg
// drop-in that appends console=hvc0 to GRUB_CMDLINE_LINUX, so the first user-
// visible boot has /dev/console wired to the VZ-captured serial device. This
// is the fix for #55.
func TestBuildCloudInit_FirstBootConsoleDropIn(t *testing.T) {
	t.Parallel()
	cfg := testConfig()

	userData, _ := BuildCloudInit(cfg, "-----BEGIN CERTIFICATE-----\nFAKE\n-----END CERTIFICATE-----\n")

	wants := []string{
		"path: /etc/default/grub.d/99_bladerunner.cfg",
		`GRUB_CMDLINE_LINUX="$GRUB_CMDLINE_LINUX console=hvc0 console=tty0"`,
	}
	for _, want := range wants {
		if !strings.Contains(userData, want) {
			t.Errorf("user-data missing expected snippet %q\n---\n%s\n---", want, userData)
		}
	}
}

// TestBuildCloudInit_FirstBootRebootSentinel verifies that the bootcmd block
// reboots exactly once, guarded by a sentinel file, so the regenerated
// grub.cfg (carrying console=hvc0) is active before runcmd executes the
// bootstrap script. Without this reboot the grub.d drop-in only takes effect
// on the second boot, which defeats the purpose.
func TestBuildCloudInit_FirstBootRebootSentinel(t *testing.T) {
	t.Parallel()
	cfg := testConfig()

	userData, _ := BuildCloudInit(cfg, "")

	wants := []string{
		"/var/lib/bladerunner/.boot1-rebooted",
		"touch /var/lib/bladerunner/.boot1-rebooted",
		"systemctl reboot",
	}
	for _, want := range wants {
		if !strings.Contains(userData, want) {
			t.Errorf("user-data missing expected sentinel/reboot snippet %q\n---\n%s\n---", want, userData)
		}
	}
}

// TestBuildCloudInit_NoLegacySedGrubEdit guards against regression: the old
// approach edited /etc/default/grub via sed in bootcmd, which only took effect
// on the second boot. The drop-in replaces it; if the sed line returns, the
// reboot we added becomes redundant and confusing.
func TestBuildCloudInit_NoLegacySedGrubEdit(t *testing.T) {
	t.Parallel()
	cfg := testConfig()

	userData, _ := BuildCloudInit(cfg, "")

	if strings.Contains(userData, "sed -i 's/^GRUB_CMDLINE_LINUX=") {
		t.Errorf("user-data still contains legacy sed grub edit; should be replaced by 99_bladerunner.cfg drop-in\n---\n%s\n---", userData)
	}
}

// TestBuildCloudInit_VsockSSHBridgeBeforeIncusInstall guards the recovery fix:
// the vsock SSH bridge unit must be created and enabled BEFORE the fragile,
// network-heavy incus package install. Otherwise a failure installing/setting
// up incus (or a transient apt error) aborts the bootstrap before the bridge
// exists, permanently leaving the guest with no host<->guest SSH over vsock
// (runcmd is once-per-instance and never retries). This is the root cause of a
// guest that boots fine but where `runner shell` resets with errno 54.
func TestBuildCloudInit_VsockSSHBridgeBeforeIncusInstall(t *testing.T) {
	t.Parallel()
	cfg := testConfig()

	userData, _ := BuildCloudInit(cfg, "")

	bridgeIdx := strings.Index(userData, "/etc/systemd/system/bladerunner-vsock-ssh.service")
	incusIdx := strings.Index(userData, "incus incus-client")
	if bridgeIdx < 0 {
		t.Fatalf("user-data does not create the vsock SSH bridge unit\n---\n%s\n---", userData)
	}
	if incusIdx < 0 {
		t.Fatalf("user-data does not install incus (test assumption broke)\n---\n%s\n---", userData)
	}
	if bridgeIdx > incusIdx {
		t.Errorf("vsock SSH bridge (idx %d) is created AFTER incus install (idx %d); it must come first so SSH survives incus/apt failures", bridgeIdx, incusIdx)
	}
}

// TestBuildCloudInit_AptUpdateResilient verifies apt-get update is retried and
// non-fatal, so a transient mirror failure (the observed trixie-security "does
// not have a Release file") does not abort the whole provisioning under
// `set -e`.
func TestBuildCloudInit_AptUpdateResilient(t *testing.T) {
	t.Parallel()
	cfg := testConfig()

	userData, _ := BuildCloudInit(cfg, "")

	wants := []string{
		"apt_update_retry",       // retry helper is defined and used
		"continuing best-effort", // helper is non-fatal (returns success after retries)
	}
	for _, want := range wants {
		if !strings.Contains(userData, want) {
			t.Errorf("user-data missing apt-resilience snippet %q\n---\n%s\n---", want, userData)
		}
	}
	// The bare, fatal `apt-get update -qq` at top level must be gone (it may
	// only appear inside the retry helper).
	if strings.Count(userData, "apt-get update -qq") > strings.Count(userData, "apt_update_retry") {
		// Heuristic: every remaining `apt-get update -qq` should be the one
		// inside the helper; callers use apt_update_retry instead.
		t.Errorf("user-data still has direct fatal `apt-get update -qq` calls; route them through apt_update_retry\n---\n%s\n---", userData)
	}
}

// TestBuildCloudInit_UpdateGrubStillRuns ensures bootcmd still regenerates
// /boot/grub/grub.cfg so the drop-in is picked up before the sentinel reboot.
func TestBuildCloudInit_UpdateGrubStillRuns(t *testing.T) {
	t.Parallel()
	cfg := testConfig()

	userData, _ := BuildCloudInit(cfg, "")

	if !strings.Contains(userData, "update-grub") {
		t.Errorf("user-data missing update-grub invocation in bootcmd\n---\n%s\n---", userData)
	}
}
