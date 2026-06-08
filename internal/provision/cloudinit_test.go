package provision

import (
	"os"
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
		LocalNTPPort:  15557,
		VsockNTPPort:  18557,
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

// TestBuildCloudInit_ShareAutomountWhenEnabled verifies that, with a share
// configured, the bootstrap emits the VirtioFS mount (matching tag), the nofail
// option (boot survives an absent share), and the ACPI poweroff pin that makes
// `runner eject` a deterministic clean shutdown.
func TestBuildCloudInit_ShareAutomountWhenEnabled(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.ShareDir = "/some/host/dir"
	cfg.ShareTag = config.DefaultShareTag

	userData, _ := BuildCloudInit(cfg, "")

	wants := []string{
		"Type=virtiofs",                  // mount unit type
		"What=" + config.DefaultShareTag, // mounts the configured tag
		"Where=" + config.DefaultShareGuestPath,
		"mnt-share.mount", // escaped unit filename
		"nofail",          // boot survives an absent share
		config.DefaultShareTag + " " + config.DefaultShareGuestPath + " virtiofs", // fstab line
		"HandlePowerKey=poweroff", // ACPI poweroff pin for deterministic eject
		"modprobe virtiofs",       // module load best-effort
	}
	for _, want := range wants {
		if !strings.Contains(userData, want) {
			t.Errorf("user-data missing share snippet %q\n---\n%s\n---", want, userData)
		}
	}
}

// TestBuildCloudInit_ShareHonorsGuestPath verifies a non-default Share.GuestPath
// actually drives the emitted mount (the unit filename, Where=, and fstab line),
// not just the pack report.
func TestBuildCloudInit_ShareHonorsGuestPath(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.ShareDir = "/some/host/dir"
	cfg.ShareTag = config.DefaultShareTag
	cfg.ShareGuestPath = "/srv/data"

	userData, _ := BuildCloudInit(cfg, "")

	wants := []string{
		"Where=/srv/data",
		"srv-data.mount", // escaped unit filename for /srv/data
		config.DefaultShareTag + " /srv/data virtiofs",
	}
	for _, want := range wants {
		if !strings.Contains(userData, want) {
			t.Errorf("user-data did not honor custom guest path %q\n---\n%s\n---", want, userData)
		}
	}
	if strings.Contains(userData, "mnt-share.mount") {
		t.Error("expected the custom guest path to replace the default /mnt/share unit")
	}
}

// TestBuildCloudInit_NoShareWhenDisabled verifies that with no share configured
// (the default for plain start/boot) none of the share/ACPI machinery is emitted,
// so non-cartridge boots are unchanged.
func TestBuildCloudInit_NoShareWhenDisabled(t *testing.T) {
	t.Parallel()
	cfg := testConfig() // ShareDir empty

	userData, _ := BuildCloudInit(cfg, "")

	unwanted := []string{
		"Type=virtiofs",
		"mnt-share.mount",
		"HandlePowerKey=poweroff",
		"modprobe virtiofs",
	}
	for _, bad := range unwanted {
		if strings.Contains(userData, bad) {
			t.Errorf("user-data unexpectedly contains share snippet %q when sharing is disabled\n---\n%s\n---", bad, userData)
		}
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

// TestBuildCloudInit_ChronyInstalled verifies chrony is added to the same
// resilient core apt install line as socat/jq, so it is present (with retries)
// before the fragile incus provisioning.
func TestBuildCloudInit_ChronyInstalled(t *testing.T) {
	t.Parallel()
	cfg := testConfig()

	userData, _ := BuildCloudInit(cfg, "")

	if !strings.Contains(userData, "openssh-server socat jq chrony") {
		t.Errorf("user-data does not install chrony in the core apt line\n---\n%s\n---", userData)
	}
}

// TestBuildCloudInit_ChronyInstallsInCoreBlockBeforeIncus guards that chrony is
// installed in the early, retried, resilient core-package block — NOT in the
// best-effort incus block where a failure is swallowed by `|| true`. A chrony
// install that lands in the swallowed block could silently never happen while
// timesyncd is masked, stranding the guest with zero time sync.
func TestBuildCloudInit_ChronyInstallsInCoreBlockBeforeIncus(t *testing.T) {
	t.Parallel()
	cfg := testConfig()

	userData, _ := BuildCloudInit(cfg, "")

	chronyIdx := strings.Index(userData, "openssh-server socat jq chrony")
	incusIdx := strings.Index(userData, "incus incus-client")
	if chronyIdx < 0 {
		t.Fatalf("user-data does not install chrony in the core line\n---\n%s\n---", userData)
	}
	if incusIdx < 0 {
		t.Fatalf("user-data does not install incus (test assumption broke)\n---\n%s\n---", userData)
	}
	if chronyIdx > incusIdx {
		t.Errorf("chrony (idx %d) installs AFTER incus (idx %d); it must be in the early resilient core block", chronyIdx, incusIdx)
	}
}

// TestBuildCloudInit_ChronyConfEmitted verifies the suspend-tuned chrony.conf is
// written, with the unlimited-step makestep and rtcsync directives.
func TestBuildCloudInit_ChronyConfEmitted(t *testing.T) {
	t.Parallel()
	cfg := testConfig()

	userData, _ := BuildCloudInit(cfg, "")

	wants := []string{
		"/etc/chrony/chrony.conf",
		"makestep 1.0 -1",
		"rtcsync",
		// Host-pointed source over vsock (Step 2): the guest coheres to the host
		// clock, not UTC, and works offline.
		"server 127.0.0.1 iburst prefer minpoll 4 maxpoll 6",
	}
	for _, want := range wants {
		if !strings.Contains(userData, want) {
			t.Errorf("user-data missing chrony.conf snippet %q\n---\n%s\n---", want, userData)
		}
	}
	// The public NTP pool must be DROPPED: it needs internet (fails in airplane
	// mode) and syncs to UTC instead of the host clock.
	if strings.Contains(userData, "pool 2.debian.pool.ntp.org") {
		t.Errorf("user-data still contains the dropped public NTP pool\n---\n%s\n---", userData)
	}
}

// TestBuildCloudInit_NTPBridgeEmitted verifies the guest socat UDP<->vsock NTP
// bridge unit is written and enabled, relaying guest localhost UDP 123 to the
// host SNTP responder over vsock (CID 2, VsockNTPPort).
func TestBuildCloudInit_NTPBridgeEmitted(t *testing.T) {
	t.Parallel()
	cfg := testConfig()

	userData, _ := BuildCloudInit(cfg, "")

	wants := []string{
		"/etc/systemd/system/bladerunner-vsock-ntp.service",
		"socat UDP4-RECVFROM:123,bind=127.0.0.1,fork,reuseaddr VSOCK-CONNECT:2:18557",
		"systemctl enable --now bladerunner-vsock-ntp.service",
	}
	for _, want := range wants {
		if !strings.Contains(userData, want) {
			t.Errorf("user-data missing NTP bridge snippet %q\n---\n%s\n---", want, userData)
		}
	}
}

// TestChronyConfMatchesCheckedInFile is a drift guard: the chronyConf const (the
// cloud-init path) and scripts/chrony.conf (the baked-image path) are synced by
// hand. They must stay byte-identical or the two provisioning paths diverge.
func TestChronyConfMatchesCheckedInFile(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("../../scripts/chrony.conf")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != chronyConf {
		t.Errorf("chronyConf const drifted from scripts/chrony.conf")
	}
}

// TestBuildCloudInit_TimesyncdMaskedAfterChronyActive verifies systemd-timesyncd
// is masked, AND that the mask is gated behind an `is-active chrony` check that
// precedes it — the half-removal guard that prevents a failed chrony install
// from leaving the guest with no time sync at all.
func TestBuildCloudInit_TimesyncdMaskedAfterChronyActive(t *testing.T) {
	t.Parallel()
	cfg := testConfig()

	userData, _ := BuildCloudInit(cfg, "")

	guardIdx := strings.Index(userData, "systemctl is-active --quiet chrony")
	maskIdx := strings.Index(userData, "systemctl mask systemd-timesyncd")
	if guardIdx < 0 {
		t.Fatalf("user-data missing the `is-active chrony` half-removal guard\n---\n%s\n---", userData)
	}
	if maskIdx < 0 {
		t.Fatalf("user-data does not mask systemd-timesyncd\n---\n%s\n---", userData)
	}
	if guardIdx > maskIdx {
		t.Errorf("timesyncd mask (idx %d) is not gated behind the chrony-active check (idx %d)", maskIdx, guardIdx)
	}
}

// TestBuildCloudInit_WatchdogEmitted verifies the guest-local wake-heal watchdog
// script + unit are written and enabled, and that the watchdog heals the clock
// and the two stateless socat relays.
func TestBuildCloudInit_WatchdogEmitted(t *testing.T) {
	t.Parallel()
	cfg := testConfig()

	userData, _ := BuildCloudInit(cfg, "")

	wants := []string{
		"/usr/local/sbin/bladerunner-watchdog.sh",
		"/etc/systemd/system/bladerunner-watchdog.service",
		// burst forces an immediate host re-measurement so the clock heal is
		// bounded by the watchdog loop, not chrony's autonomous poll.
		"chronyc burst 4/4",
		"chronyc makestep",
		"bladerunner-vsock-incus",
		"bladerunner-vsock-oidc",
		"systemctl enable --now bladerunner-watchdog.service",
	}
	for _, want := range wants {
		if !strings.Contains(userData, want) {
			t.Errorf("user-data missing watchdog snippet %q\n---\n%s\n---", want, userData)
		}
	}
}

// TestBuildCloudInit_WatchdogNeverRestartsNetworkd guards the container-safety
// constraint: the watchdog must never blanket-restart systemd-networkd (it would
// tear down running Incus container bridges).
func TestBuildCloudInit_WatchdogNeverRestartsNetworkd(t *testing.T) {
	t.Parallel()
	cfg := testConfig()

	userData, _ := BuildCloudInit(cfg, "")

	if strings.Contains(userData, "systemctl restart systemd-networkd") {
		t.Errorf("watchdog must NEVER restart systemd-networkd (disrupts Incus container bridges)\n---\n%s\n---", userData)
	}
}

// TestBuildCloudInit_WatchdogLogsEveryCycle locks in the "log even when healthy"
// requirement: every cycle the watchdog logs the clock offset and the RTC delta
// to the journal so the NEXT wedge yields measurement, not inference.
func TestBuildCloudInit_WatchdogLogsEveryCycle(t *testing.T) {
	t.Parallel()
	cfg := testConfig()

	userData, _ := BuildCloudInit(cfg, "")

	wants := []string{
		"logger -t \"$TAG\"", // journal logging via the bladerunner-watchdog tag
		"TAG=bladerunner-watchdog",
		"sys_offset", // clock-offset observation logged
		"rtc_delta",  // RTC-vs-realtime delta logged (the VZ-RTC empirical test)
	}
	for _, want := range wants {
		if !strings.Contains(userData, want) {
			t.Errorf("user-data missing watchdog per-cycle log token %q\n---\n%s\n---", want, userData)
		}
	}
}

// TestProvisioningAssetsMatchCheckedInFiles guards against drift between the
// cloud-init path (which embeds these as Go consts) and the image-build path
// (which --copy-ins the checked-in scripts/ files). The two MUST stay
// byte-identical or a cloud-init guest and an image-built guest would run
// different time-heal logic. If this fails, update both copies together.
func TestProvisioningAssetsMatchCheckedInFiles(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		path string
		got  string
	}{
		{"chrony.conf", "../../scripts/chrony.conf", chronyConf},
		{"bladerunner-watchdog.sh", "../../scripts/bladerunner-watchdog.sh", watchdogScript},
		{"bladerunner-watchdog.service", "../../scripts/bladerunner-watchdog.service", watchdogUnit},
	}
	for _, c := range cases {
		want, err := os.ReadFile(c.path)
		if err != nil {
			t.Fatalf("read %s: %v", c.path, err)
		}
		if string(want) != c.got {
			t.Errorf("%s drifted from %s — the cloud-init const and the image-build file must be byte-identical (update both)", c.name, c.path)
		}
	}
}
