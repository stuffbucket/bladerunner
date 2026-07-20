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
		LocalNTPPort:  15557,
		VsockNTPPort:  18557,
	}
}

// TestBuildCloudInit_FirstBootConsoleDropIn verifies that the user-data
// emitted by BuildCloudInit installs the /etc/default/grub.d/99_bladerunner.cfg
// drop-in that appends console=hvc0 to GRUB_CMDLINE_LINUX, so the KERNEL's own
// console is wired to the VZ-captured serial device from the next natural boot
// onward (#55). First-boot userspace breadcrumbs reach hvc0 directly without
// waiting for this (#57).
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

// TestBuildCloudInit_NoFirstBootReboot guards #57: there must be NO forced
// first-boot reboot. The old bootcmd reboot (touch .boot1-rebooted +
// systemctl reboot) never fired reliably and doubled cold-boot time. First-boot
// visibility now comes from br_stage writing straight to /dev/hvc0, so the
// reboot is unnecessary and must not return.
func TestBuildCloudInit_NoFirstBootReboot(t *testing.T) {
	t.Parallel()
	cfg := testConfig()

	userData, _ := BuildCloudInit(cfg, "")

	forbidden := []string{
		".boot1-rebooted",
		"systemctl reboot",
		"shutdown -r now",
	}
	for _, bad := range forbidden {
		if strings.Contains(userData, bad) {
			t.Errorf("user-data still contains forced first-boot reboot snippet %q; #57 reboot must be gone\n---\n%s\n---", bad, userData)
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
// the vsock SSH relay must be created and enabled BEFORE the fragile,
// network-heavy incus package install. Otherwise a failure installing/setting
// up incus (or a transient apt error) aborts the bootstrap before the relay
// exists, permanently leaving the guest with no host<->guest SSH over vsock
// (runcmd is once-per-instance and never retries). This is the root cause of a
// guest that boots fine but where `br shell` resets with errno 54. The ssh
// channel is now an instance of the shared relay template, whose per-channel
// arg file names the ssh backend.
func TestBuildCloudInit_VsockSSHBridgeBeforeIncusInstall(t *testing.T) {
	t.Parallel()
	cfg := testConfig()

	userData, _ := BuildCloudInit(cfg, "")

	bridgeIdx := strings.Index(userData, "/etc/bladerunner/relays/ssh.env")
	incusIdx := strings.Index(userData, "incus incus-client")
	if bridgeIdx < 0 {
		t.Fatalf("user-data does not create the vsock SSH relay arg file\n---\n%s\n---", userData)
	}
	if incusIdx < 0 {
		t.Fatalf("user-data does not install incus (test assumption broke)\n---\n%s\n---", userData)
	}
	if bridgeIdx > incusIdx {
		t.Errorf("vsock SSH relay (idx %d) is created AFTER incus install (idx %d); it must come first so SSH survives incus/apt failures", bridgeIdx, incusIdx)
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

// TestBuildCloudInit_BootBreadcrumbs verifies the bootstrap emits the br_stage
// console-breadcrumb helper and the milestone calls, so a stuck first boot is
// triageable from the host-side console.log before SSH is even up. (#52)
func TestBuildCloudInit_BootBreadcrumbs(t *testing.T) {
	t.Parallel()
	cfg := testConfig()

	userData, _ := BuildCloudInit(cfg, "")

	wants := []string{
		"br_stage() {",   // helper defined
		">/dev/hvc0",     // writes straight to the VZ-captured virtio console
		"br_stage start", // first milestone
		"br_stage apt-install-incus",
		"br_stage incus-ready",
		"br_stage bootstrap-done", // last milestone
	}
	for _, want := range wants {
		if !strings.Contains(userData, want) {
			t.Errorf("user-data missing boot breadcrumb %q\n---\n%s\n---", want, userData)
		}
	}
}

// TestBuildCloudInit_ShareAutomountWhenEnabled verifies that, with a share
// configured, the bootstrap emits the VirtioFS mount (matching tag), the nofail
// option (boot survives an absent share), and the ACPI poweroff pin that makes
// `br eject` a deterministic clean shutdown.
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
// /boot/grub/grub.cfg so the 99_bladerunner.cfg drop-in routes the kernel
// console to hvc0 on the next natural boot (no forced reboot needed; see #57).
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
// bridge is emitted as an instance of the shared relay template: its per-channel
// arg file carries the exact UDP4-RECVFROM socat line, and the instance is
// enabled. It relays guest localhost UDP 123 to the host SNTP responder over
// vsock (CID 2, VsockNTPPort).
func TestBuildCloudInit_NTPBridgeEmitted(t *testing.T) {
	t.Parallel()
	cfg := testConfig()

	userData, _ := BuildCloudInit(cfg, "")

	wants := []string{
		"/etc/bladerunner/relays/ntp.env",
		"RELAY_ARGS=UDP4-RECVFROM:123,bind=127.0.0.1,fork,reuseaddr VSOCK-CONNECT:2:18557",
		"bladerunner-vsock-relay@ntp.service",
	}
	for _, want := range wants {
		if !strings.Contains(userData, want) {
			t.Errorf("user-data missing NTP bridge snippet %q\n---\n%s\n---", want, userData)
		}
	}
}

// TestBuildCloudInit_AllFourRelayChannels is the regression guard for the relay
// consolidation (#137): it asserts that the rendered cloud-init installs the
// single relay template unit and all FOUR channels (ssh/incus/oidc/ntp) with
// byte-identical socat invocations and ports to the standalone units they
// replaced. A boot-critical regression (a dropped channel, a mangled socat
// address pair, a wrong port) is caught here before the real boot-verify.
func TestBuildCloudInit_AllFourRelayChannels(t *testing.T) {
	t.Parallel()
	cfg := testConfig()

	userData, _ := BuildCloudInit(cfg, "")

	// One template unit, exec'ing socat with the word-split $RELAY_ARGS argv.
	tmplWants := []string{
		"/etc/systemd/system/bladerunner-vsock-relay@.service",
		"EnvironmentFile=/etc/bladerunner/relays/%i.env",
		"ExecStart=/usr/bin/socat $RELAY_ARGS",
	}
	for _, want := range tmplWants {
		if !strings.Contains(userData, want) {
			t.Errorf("rendered cloud-init missing relay template snippet %q\n---\n%s\n---", want, userData)
		}
	}

	// The four channels' exact socat address pairs + ports, exactly as the old
	// per-channel units invoked socat. FORWARD channels listen on vsock and
	// forward to a guest-local TCP backend; REVERSE channels listen locally and
	// dial OUT to the host over vsock (CID 2).
	channelWants := map[string]string{
		"ssh":   "RELAY_ARGS=VSOCK-LISTEN:10022,fork,reuseaddr TCP:127.0.0.1:22",
		"incus": "RELAY_ARGS=VSOCK-LISTEN:18443,fork,reuseaddr TCP:127.0.0.1:8443",
		"oidc":  "RELAY_ARGS=TCP-LISTEN:15556,bind=127.0.0.1,fork,reuseaddr VSOCK-CONNECT:2:18556",
		"ntp":   "RELAY_ARGS=UDP4-RECVFROM:123,bind=127.0.0.1,fork,reuseaddr VSOCK-CONNECT:2:18557",
	}
	for name, args := range channelWants {
		envPath := "/etc/bladerunner/relays/" + name + ".env"
		if !strings.Contains(userData, envPath) {
			t.Errorf("rendered cloud-init missing %s arg file %q\n---\n%s\n---", name, envPath, userData)
		}
		if !strings.Contains(userData, args) {
			t.Errorf("rendered cloud-init missing %s socat args %q (byte-identical socat semantics + ports must be preserved)\n---\n%s\n---", name, args, userData)
		}
		instance := "bladerunner-vsock-relay@" + name + ".service"
		if !strings.Contains(userData, instance) {
			t.Errorf("rendered cloud-init does not enable %s instance %q\n---\n%s\n---", name, instance, userData)
		}
	}

	// The ssh + incus FORWARD channels keep their backend spin-wait; the oidc +
	// ntp REVERSE channels do not (they dial out and have no local backend).
	waitWants := []string{"RELAY_WAIT=22", "RELAY_WAIT=8443"}
	for _, want := range waitWants {
		if !strings.Contains(userData, want) {
			t.Errorf("rendered cloud-init missing backend spin-wait %q\n---\n%s\n---", want, userData)
		}
	}

	// The legacy standalone units a pre-baked image ships enabled must be
	// superseded (masked) so a template instance is the sole owner of each port.
	if !strings.Contains(userData, `systemctl mask "bladerunner-vsock-$legacy.service"`) {
		t.Errorf("rendered cloud-init does not mask the legacy standalone relay units; a baked image could double-bind a channel's port\n---\n%s\n---", userData)
	}

	// The old four inline heredoc units must be gone: no standalone
	// bladerunner-vsock-<name>.service should be WRITTEN any more (they may only
	// appear as the masked legacy names in the supersede loop / template@ form).
	forbidden := []string{
		"/etc/systemd/system/bladerunner-vsock-ssh.service",
		"/etc/systemd/system/bladerunner-vsock-incus.service",
		"/etc/systemd/system/bladerunner-vsock-oidc.service",
		"/etc/systemd/system/bladerunner-vsock-ntp.service",
	}
	for _, bad := range forbidden {
		if strings.Contains(userData, bad) {
			t.Errorf("rendered cloud-init still writes the standalone unit %q; it must be consolidated into the relay template\n---\n%s\n---", bad, userData)
		}
	}
}

// TestBuildCloudInit_RelayPortsThreadThrough proves the four channels' socat
// lines are rendered from cfg (not hardcoded): a non-default port config must
// show up in the corresponding RELAY_ARGS line. This guards against a future
// edit that accidentally freezes a port literal.
func TestBuildCloudInit_RelayPortsThreadThrough(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.VsockSSHPort = 20022
	cfg.VsockAPIPort = 28443
	cfg.LocalOIDCPort = 25556
	cfg.VsockOIDCPort = 28556
	cfg.VsockNTPPort = 28557

	userData, _ := BuildCloudInit(cfg, "")

	wants := []string{
		"RELAY_ARGS=VSOCK-LISTEN:20022,fork,reuseaddr TCP:127.0.0.1:22",
		"RELAY_ARGS=VSOCK-LISTEN:28443,fork,reuseaddr TCP:127.0.0.1:8443",
		"RELAY_ARGS=TCP-LISTEN:25556,bind=127.0.0.1,fork,reuseaddr VSOCK-CONNECT:2:28556",
		"RELAY_ARGS=UDP4-RECVFROM:123,bind=127.0.0.1,fork,reuseaddr VSOCK-CONNECT:2:28557",
	}
	for _, want := range wants {
		if !strings.Contains(userData, want) {
			t.Errorf("non-default port config did not thread into the relay socat line %q\n---\n%s\n---", want, userData)
		}
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
// and the vsock relays (now instances of the shared relay template, healed in
// one loop over the channel table).
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
		// the relays are healed via the template instance, in one loop
		"bladerunner-vsock-relay@$name.service",
		`relays="incus 8443 oidc - ntp - ssh 22"`,
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
// requirement: every cycle the watchdog logs the clock offset to the journal so
// the NEXT wedge yields measurement, not inference.
func TestBuildCloudInit_WatchdogLogsEveryCycle(t *testing.T) {
	t.Parallel()
	cfg := testConfig()

	userData, _ := BuildCloudInit(cfg, "")

	wants := []string{
		"logger -t \"$TAG\"", // journal logging via the bladerunner-watchdog tag
		"TAG=bladerunner-watchdog",
		"sys_offset", // clock-offset observation logged
	}
	for _, want := range wants {
		if !strings.Contains(userData, want) {
			t.Errorf("user-data missing watchdog per-cycle log token %q\n---\n%s\n---", want, userData)
		}
	}
}

// TestVsockNTPUnitRendersDefaultPortVerbatim was removed with the standalone
// bladerunner-vsock-ntp.service embed: the NTP channel is now the @ntp instance
// of the shared relay template, so its port-threading is covered by
// TestBuildCloudInit_RelayPortsThreadThrough (non-default VsockNTPPort) and the
// exact-line assertion in TestBuildCloudInit_AllFourRelayChannels.
