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
