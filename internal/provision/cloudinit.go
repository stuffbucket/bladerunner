package provision

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/logging"
	"github.com/stuffbucket/bladerunner/internal/util"
)

func BuildCloudInit(cfg *config.Config, clientCertPEM string) (string, string) {
	bootstrapScript := renderBootstrapScript(cfg)

	var b strings.Builder
	b.WriteString("#cloud-config\n")
	fmt.Fprintf(&b, "hostname: %s\n", cfg.Hostname)
	b.WriteString("manage_etc_hosts: true\n")
	aptMirror := config.DefaultAptMirrorURI(cfg.Arch)
	b.WriteString("apt:\n")
	b.WriteString("  primary:\n")
	b.WriteString("    - arches: [default]\n")
	fmt.Fprintf(&b, "      uri: %s\n", aptMirror)
	b.WriteString("  security:\n")
	b.WriteString("    - arches: [default]\n")
	fmt.Fprintf(&b, "      uri: %s\n", aptMirror)
	b.WriteString("users:\n")
	b.WriteString("  - default\n")
	fmt.Fprintf(&b, "  - name: %s\n", cfg.SSHUser)
	b.WriteString("    shell: /bin/bash\n")
	b.WriteString("    sudo: ALL=(ALL) NOPASSWD:ALL\n")
	b.WriteString("    groups: [sudo]\n")
	b.WriteString("    lock_passwd: false\n")
	b.WriteString("    ssh_authorized_keys:\n")
	fmt.Fprintf(&b, "      - %s\n", cfg.SSHPublicKey)
	b.WriteString("chpasswd:\n")
	b.WriteString("  expire: false\n")
	b.WriteString("  users:\n")
	fmt.Fprintf(&b, "    - name: %s\n", cfg.SSHUser)
	b.WriteString("      password: bladerunner\n")
	b.WriteString("      type: text\n")
	b.WriteString("write_files:\n")
	b.WriteString("  - path: /var/lib/bladerunner/host-client.crt\n")
	b.WriteString("    permissions: '0644'\n")
	b.WriteString("    content: |\n")
	b.WriteString(indent(clientCertPEM, 6))
	b.WriteString("  - path: /usr/local/sbin/bladerunner-bootstrap.sh\n")
	b.WriteString("    permissions: '0755'\n")
	b.WriteString("    content: |\n")
	b.WriteString(indent(bootstrapScript, 6))
	// Drop-in grub override: routes the kernel's own console to hvc0 (the
	// VZ-captured serial device) on every subsequent natural boot. cloud-init
	// applies write_files before bootcmd/runcmd, so the file is in place when
	// update-grub runs below. This file APPENDS to GRUB_CMDLINE_LINUX rather than
	// replacing it, so any existing distro defaults are preserved.
	b.WriteString("  - path: /etc/default/grub.d/99_bladerunner.cfg\n")
	b.WriteString("    permissions: '0644'\n")
	b.WriteString("    content: |\n")
	b.WriteString("      GRUB_CMDLINE_LINUX=\"$GRUB_CMDLINE_LINUX console=hvc0 console=tty0\"\n")
	b.WriteString("bootcmd:\n")
	b.WriteString("  # Regenerate grub config so the 99_bladerunner.cfg drop-in (written by\n")
	b.WriteString("  # write_files above, which cloud-init applies before bootcmd) lands in\n")
	b.WriteString("  # /boot/grub/grub.cfg, routing the KERNEL's console to hvc0 from the next\n")
	b.WriteString("  # natural boot onward. We deliberately do NOT force a first-boot reboot:\n")
	b.WriteString("  # the bootstrap's br_stage breadcrumbs write straight to /dev/hvc0, which\n")
	b.WriteString("  # is present in late userspace regardless of the kernel console= cmdline,\n")
	b.WriteString("  # so first-boot progress is already visible. The old bootcmd reboot never\n")
	b.WriteString("  # fired reliably and doubled cold-boot time (#57).\n")
	b.WriteString("  - [sh, -c, 'update-grub || grub-mkconfig -o /boot/grub/grub.cfg || true']\n")
	b.WriteString("growpart:\n")
	b.WriteString("  mode: auto\n")
	b.WriteString("  devices: [/]\n")
	b.WriteString("  ignore_growroot_disabled: false\n")
	b.WriteString("resize_rootfs: true\n")
	b.WriteString("runcmd:\n")
	b.WriteString("  - [bash, /usr/local/sbin/bladerunner-bootstrap.sh]\n")

	metaData := fmt.Sprintf("instance-id: bladerunner-%s\nlocal-hostname: %s\n", cfg.Name, cfg.Hostname)
	return b.String(), metaData
}

func WriteSeedFiles(cfg *config.Config, userData, metaData string) error {
	start := time.Now()
	if err := os.MkdirAll(cfg.CloudInitDir, 0o755); err != nil {
		return fmt.Errorf("create cloud-init dir: %w", err)
	}

	if err := os.WriteFile(filepath.Join(cfg.CloudInitDir, "user-data"), []byte(userData), 0o644); err != nil {
		return fmt.Errorf("write user-data: %w", err)
	}
	if err := os.WriteFile(filepath.Join(cfg.CloudInitDir, "meta-data"), []byte(metaData), 0o644); err != nil {
		return fmt.Errorf("write meta-data: %w", err)
	}

	logging.L().Info("cloud-init seed files written", "dir", cfg.CloudInitDir, "elapsed", time.Since(start).Round(time.Millisecond).String())
	return nil
}

func BuildCloudInitISO(ctx context.Context, cfg *config.Config) error {
	start := time.Now()
	if err := os.MkdirAll(filepath.Dir(cfg.CloudInitISO), 0o755); err != nil {
		return fmt.Errorf("create cloud-init ISO parent: %w", err)
	}

	_ = os.Remove(cfg.CloudInitISO)
	baseOut := strings.TrimSuffix(cfg.CloudInitISO, filepath.Ext(cfg.CloudInitISO))

	cmd := exec.CommandContext(ctx,
		"hdiutil", "makehybrid",
		"-o", baseOut,
		cfg.CloudInitDir,
		"-iso", "-joliet",
		"-default-volume-name", "cidata",
	)
	logging.L().Info("building cloud-init ISO", "input_dir", cfg.CloudInitDir, "output", cfg.CloudInitISO)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("create cloud-init iso with hdiutil: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	candidates := []string{baseOut, baseOut + ".iso", baseOut + ".cdr", cfg.CloudInitISO}
	for _, c := range candidates {
		if util.FileExists(c) {
			if c != cfg.CloudInitISO {
				_ = os.Remove(cfg.CloudInitISO)
				if err := os.Rename(c, cfg.CloudInitISO); err != nil {
					return fmt.Errorf("rename cloud-init iso from %s: %w", c, err)
				}
			}
			logging.L().Info("cloud-init ISO built", "path", cfg.CloudInitISO, "elapsed", time.Since(start).Round(time.Millisecond).String())
			return nil
		}
	}

	return fmt.Errorf("cloud-init ISO not produced at expected paths (wanted %s)", cfg.CloudInitISO)
}

func renderBootstrapScript(cfg *config.Config) string {
	return fmt.Sprintf(`#!/usr/bin/env bash
set -euxo pipefail
export DEBIAN_FRONTEND=noninteractive

mkdir -p /var/lib/bladerunner

# Emit a host-visible breadcrumb to the VZ-captured virtio console so first-boot
# progress is visible even before SSH/Incus are up (#52, #57). Write straight to
# /dev/hvc0 -- the virtio-console device VZ streams into the host-side
# console.log -- because it is present in late userspace regardless of the
# kernel console= cmdline. /dev/console only routes here once the
# 99_bladerunner.cfg grub drop-in takes effect (the next natural boot), so it is
# only a fallback. Best-effort: a missing device must never abort the bootstrap.
br_stage() {
  msg="bladerunner-bootstrap: stage=$1 t=$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)"
  echo "$msg" >/dev/hvc0 2>/dev/null || echo "$msg" >/dev/console 2>/dev/null || true
}
br_stage start

# Load vsock kernel modules early (required for socat VSOCK-LISTEN)
# These may be built-in on some kernels, so ignore errors.
modprobe vsock 2>/dev/null || true
modprobe vmw_vsock_virtio_transport 2>/dev/null || true
modprobe vhost_vsock 2>/dev/null || true

# Verify vsock is available
if [ ! -e /dev/vsock ]; then
  echo "WARNING: /dev/vsock not found, vsock forwarding may not work" >&2
fi

# Resilient apt update: retry transient mirror failures (e.g. a freshly
# promoted trixie-security that briefly has no Release file) and never abort
# the whole bootstrap on apt. The host<->guest vsock SSH bridge created below
# is more important than fully up-to-date package indexes; if it dies here,
# runcmd is once-per-instance and never retries, permanently stranding SSH.
apt_update_retry() {
  for attempt in 1 2 3 4 5; do
    if apt-get update -qq; then
      return 0
    fi
    echo "bladerunner: apt-get update failed (attempt ${attempt}/5), retrying" >&2
    sleep 3
  done
  echo "bladerunner: apt-get update still failing; continuing best-effort" >&2
  return 0
}

# Zabbly fallback: dormant for the default Debian 13 (trixie) image, which ships
# incus and incus-client in main. Retained for Ubuntu and other distros reached
# via --image-url where the native package is not available.
install_zabbly_repo() {
  if [ ! -e /etc/apt/keyrings/zabbly.asc ]; then
    mkdir -p /etc/apt/keyrings
    curl -fsSL https://pkgs.zabbly.com/key.asc -o /etc/apt/keyrings/zabbly.asc
  fi

  codename=""
  if [ -r /etc/os-release ]; then
    . /etc/os-release
    codename="${VERSION_CODENAME:-}"
  fi

  if [ -z "$codename" ]; then
    codename="noble"
  fi

  cat >/etc/apt/sources.list.d/zabbly-incus-stable.sources <<SRC
Enabled: yes
Types: deb
URIs: https://pkgs.zabbly.com/incus/stable
Suites: ${codename}
Components: main
Architectures: $(dpkg --print-architecture)
Signed-By: /etc/apt/keyrings/zabbly.asc
SRC
}

# Detect Debian trixie (or any distro shipping incus in its native repos) so we
# can skip the Zabbly fallback entirely.
native_incus_distro() {
  if [ ! -r /etc/os-release ]; then
    return 1
  fi
  . /etc/os-release
  case "${ID:-}:${VERSION_CODENAME:-}" in
    debian:trixie|debian:forky|debian:sid)
      return 0
      ;;
  esac
  return 1
}

# --- Critical control-path packages FIRST, resiliently. socat + sshd are all
#     the host<->guest vsock SSH bridge needs; install them (with retries)
#     before the heavier, failure-prone incus provisioning below.
if command -v apt-get >/dev/null 2>&1; then
  br_stage apt-update
  apt_update_retry
  br_stage apt-install-base
  for attempt in 1 2 3; do
    if apt-get install -y -qq ca-certificates curl gpg openssh-server socat jq chrony; then
      break
    fi
    echo "bladerunner: core package install failed (attempt ${attempt}/3), retrying" >&2
    sleep 3
  done
elif command -v dnf >/dev/null 2>&1; then
  dnf install -y -q openssh-server socat jq chrony || true
fi

systemctl enable --now ssh || true
systemctl enable --now sshd || true
br_stage ssh-up

# --- Break-glass SSH access, provisioned HERE in the bootstrap (runcmd) rather
#     than via cloud-init's users/ssh_authorized_keys/chpasswd modules. Those are
#     per-instance modules that the first-boot reboot (bootcmd, #56) runs BEFORE,
#     so on this image they never apply; runcmd always runs (it installs incus
#     below), making this the reliable place to guarantee a way in even when the
#     incus provisioning that follows fails.
SSH_USER='%s'
SSH_PUBKEY='%s'
if ! id -u "$SSH_USER" >/dev/null 2>&1; then
  useradd -m -s /bin/bash "$SSH_USER" || true
fi
usermod -s /bin/bash "$SSH_USER" 2>/dev/null || true
[ -d "/home/$SSH_USER" ] || mkdir -p "/home/$SSH_USER"
mkdir -p "/home/$SSH_USER/.ssh"
printf '%%s\n' "$SSH_PUBKEY" > "/home/$SSH_USER/.ssh/authorized_keys"
chmod 700 "/home/$SSH_USER/.ssh"
chmod 600 "/home/$SSH_USER/.ssh/authorized_keys"
chown -R "$SSH_USER:$SSH_USER" "/home/$SSH_USER" 2>/dev/null || true
usermod -aG sudo "$SSH_USER" 2>/dev/null || true
echo "$SSH_USER ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/90-bladerunner
chmod 440 /etc/sudoers.d/90-bladerunner
echo "$SSH_USER:bladerunner" | chpasswd 2>/dev/null || true
# SSH password auth as a fallback escape hatch (loopback-only vsock bridge).
if [ -d /etc/ssh/sshd_config.d ]; then
  echo "PasswordAuthentication yes" > /etc/ssh/sshd_config.d/90-bladerunner.conf
fi
systemctl restart ssh 2>/dev/null || systemctl restart sshd 2>/dev/null || true

# --- vsock relay units: create + enable BEFORE incus provisioning, so a
#     failure installing/configuring incus (or any later command) can never
#     strand host<->guest SSH. ConditionPathExists guards mean an instance
#     simply waits, rather than errors, if socat / its TCP target is not up yet.
#     All four channels (ssh/incus/oidc/ntp) run as instances of ONE template
#     unit; per-channel socat args live in /etc/bladerunner/relays/<name>.env.
%s
br_stage vsock-services-up

# --- Best-effort incus provisioning. Everything below is non-fatal: if it
#     fails, host<->guest SSH (configured above) still works, so the VM stays
#     reachable and debuggable instead of silently stranding the operator.
if command -v apt-get >/dev/null 2>&1; then
  br_stage apt-install-incus
  if native_incus_distro; then
    apt-get install -y -qq incus incus-client || true
  elif ! apt-get install -y -qq incus incus-client; then
    install_zabbly_repo
    apt_update_retry
    apt-get install -y -qq incus incus-client || true
  fi
elif command -v dnf >/dev/null 2>&1; then
  dnf install -y -q incus incus-client || true
fi

if getent group incus-admin >/dev/null 2>&1; then
  usermod -a -G incus-admin %s || true
fi

systemctl enable --now incus || true
systemctl enable --now incus.socket || true
br_stage incus-socket-up

for i in $(seq 1 60); do
  if incus admin waitready --timeout=1 >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
br_stage incus-ready

incus admin init --auto || true
incus config set core.https_address "[::]:8443" || true
br_stage incus-init-done

# Configure Incus to trust the bladerunner local OIDC provider.
# The issuer URL is the loopback address inside the guest, which is forwarded
# over vsock to the bladerunner OIDC server on the host. See internal/oidc.
# NOTE: the keys are oidc.* (Incus 6.x), NOT core.oidc.* — the latter are
# rejected as unknown keys and silently swallowed by the trailing '|| true'.
incus config set oidc.issuer    "%s" || true
incus config set oidc.client.id "%s" || true
incus config set oidc.audience  "%s" || true

# Add the host client certificate to trust store (kept for the --auth=cert
# fallback path; safe to leave even when OIDC is the primary auth method).
incus config trust add-certificate /var/lib/bladerunner/host-client.crt --name bladerunner-host 2>/dev/null ||
  incus config trust add /var/lib/bladerunner/host-client.crt --name bladerunner-host 2>/dev/null ||
  echo "Note: Could not add host certificate to trust store (may already exist)"

# --- Install the Incus web UI (incus-ui-canonical, from Zabbly) as static files
#     only. We extract the .deb instead of 'apt install'-ing it so apt never
#     swaps Debian's incus for Zabbly's to satisfy its "Depends: incus". Debian
#     trixie ships no UI package. incusd serves these at /ui/ once pointed at the
#     directory via the INCUS_UI environment variable. Best-effort, non-fatal.
install_zabbly_repo || true
apt_update_retry
( cd /tmp && apt-get download incus-ui-canonical ) >/dev/null 2>&1 || true
UI_DEB=$(ls -1 /tmp/incus-ui-canonical_*.deb 2>/dev/null | head -1)
if [ -n "$UI_DEB" ]; then
  dpkg-deb -x "$UI_DEB" / || true
  mkdir -p /etc/systemd/system/incus.service.d
  printf '[Service]\nEnvironment=INCUS_UI=/opt/incus/ui\n' >/etc/systemd/system/incus.service.d/10-bladerunner-ui.conf
  systemctl daemon-reload || true
  systemctl restart incus || true
  echo "bladerunner: installed Incus web UI to /opt/incus/ui (served at /ui/)"
else
  echo "bladerunner: incus-ui-canonical download failed; web UI not installed (non-fatal)" >&2
fi

# incus should be listening now; nudge the API relay so it picks up :8443
# without waiting for its restart timer.
systemctl restart bladerunner-vsock-relay@incus.service || true

# Wait a moment for services to start
sleep 2

date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ >/var/lib/bladerunner/ready
br_stage bootstrap-done
`,
		// Break-glass SSH block (SSH_USER, SSH_PUBKEY), placed first because it
		// appears early in the bootstrap, before the vsock relays.
		cfg.SSHUser, cfg.SSHPublicKey,
		// All guest-side vsock relays (ssh/incus/oidc/ntp as ONE template unit) +
		// the chrony/watchdog time stack + the optional VirtioFS share, rendered
		// as one fragment. Ordered relays -> time-heal -> share, all before incus,
		// so the control path + time stack + backstop are in place regardless of
		// any later incus failure. Each sub-fragment is self-contained (its own
		// heredocs / port substitution), so the positional arg list here carries a
		// single %s for the whole block.
		renderVsockRelays(cfg)+renderTimeHeal(cfg)+renderShareSetup(cfg),
		cfg.SSHUser,
		cfg.OIDCIssuerURL, cfg.OIDCClientID, cfg.OIDCAudience,
	)
}

// renderVsockRelays returns the guest-side bootstrap fragment that installs the
// single templated vsock relay unit and one instance per channel
// (ssh/incus/oidc/ntp). It replaces the four near-identical inline heredoc units
// with: one template file, four tiny per-channel arg files under
// /etc/bladerunner/relays/, and one `systemctl enable --now` covering all
// instances. Every channel keeps the exact socat address pair (and ssh/incus
// their backend spin-wait) of the standalone unit it supersedes — see
// relayChannels — so socat is exec'd with byte-identical argv and ports.
//
// A pre-baked guest image (scripts/build-guest-image.sh) still ships the legacy
// standalone bladerunner-vsock-{ssh,incus,oidc,ntp} units enabled. To avoid a
// second listener double-binding a channel's port (e.g. two UDP-123 NTP
// listeners), this fragment stops+disables+masks those legacy names before
// enabling the template instances, so the template is the sole owner at runtime.
func renderVsockRelays(cfg *config.Config) string {
	var b strings.Builder

	// The single template unit. Instances read their socat args from the
	// per-channel env files written below.
	b.WriteString("mkdir -p /etc/bladerunner/relays\n")
	b.WriteString("cat >/etc/systemd/system/bladerunner-vsock-relay@.service <<'UNIT'\n")
	b.WriteString(relayTemplateUnit)
	b.WriteString("UNIT\n")

	// Per-channel arg files: RELAY_ARGS (socat address pair, byte-identical to the
	// old unit) + optional RELAY_WAIT (backend port to spin-wait for).
	for _, ch := range relayChannels(cfg) {
		fmt.Fprintf(&b, "cat >/etc/bladerunner/relays/%s.env <<'RELAYENV'\n", ch.name)
		b.WriteString(relayEnvFile(ch))
		b.WriteString("RELAYENV\n")
	}

	// Supersede the legacy standalone units a pre-baked image ships enabled, so a
	// template instance is the sole owner of each channel's port. Stop first (drop
	// any running listener), then disable+mask so they never re-activate.
	b.WriteString("for legacy in ssh incus oidc ntp; do\n")
	b.WriteString("  systemctl stop \"bladerunner-vsock-$legacy.service\" 2>/dev/null || true\n")
	b.WriteString("  systemctl disable \"bladerunner-vsock-$legacy.service\" 2>/dev/null || true\n")
	b.WriteString("  systemctl mask \"bladerunner-vsock-$legacy.service\" 2>/dev/null || true\n")
	b.WriteString("done\n")

	// Enable + start all four instances in one shot.
	b.WriteString("systemctl daemon-reload\n")
	b.WriteString("systemctl enable --now")
	for _, ch := range relayChannels(cfg) {
		fmt.Fprintf(&b, " bladerunner-vsock-relay@%s.service", ch.name)
	}
	b.WriteString(" || true\n")

	return b.String()
}

// renderTimeHeal returns the guest-side bootstrap fragment that (1) swaps
// systemd-timesyncd for chrony tuned to step the clock immediately after a host
// suspend, and (2) installs the guest-local wake-heal watchdog (script + unit).
// It is always emitted. The chrony swap is guarded:
// timesyncd is only disabled+masked AFTER chrony is verified active, so a
// transient chrony install failure never strands the guest with zero time sync.
//
// The watchdog script body + unit are the checked-in
// internal/provision/scripts/bladerunner-watchdog.{sh,service}, go:embed'd here
// (see embed.go) and --copy-in'd unchanged by the image build — one source,
// no drift. The only templated values are the port(s), threaded via the
// /etc/default/bladerunner-watchdog env file (so the script body needs no
// substitution and is emitted verbatim).
func renderTimeHeal(cfg *config.Config) string {
	var b strings.Builder

	// The vsock NTP bridge is now one instance of the shared relay template unit
	// (bladerunner-vsock-relay@ntp), emitted + enabled by renderVsockRelays, which
	// runs BEFORE this fragment. So the UDP-123 -> vsock transport the guest chrony
	// "server 127.0.0.1" line targets already exists the moment chrony enables
	// below. (renderVsockRelays also stops+masks any legacy standalone
	// bladerunner-vsock-ntp unit left on an already-published baked image, so
	// there is no double-bind during the transition to a rebuilt image.)

	// chrony.conf (no fmt verbs; safe verbatim). Replace systemd-timesyncd.
	b.WriteString("\n# --- chrony: replace systemd-timesyncd. Tuned to step the clock immediately\n")
	b.WriteString("#     after a host suspend (makestep 1.0 -1). The guest gets no paravirt\n")
	b.WriteString("#     \"you were stopped\" signal, so this guest-local NTP step is the only\n")
	b.WriteString("#     automatic recovery for post-sleep clock skew. See chrony.conf comments.\n")
	b.WriteString("cat >/etc/chrony/chrony.conf <<'CHRONY'\n")
	b.WriteString(chronyConf)
	b.WriteString("CHRONY\n")
	b.WriteString("systemctl enable --now chrony || true\n")
	// Half-removal guard: only disable+mask timesyncd once chrony is ACTIVE.
	b.WriteString("if systemctl is-active --quiet chrony; then\n")
	b.WriteString("  systemctl disable --now systemd-timesyncd 2>/dev/null || true\n")
	b.WriteString("  systemctl mask systemd-timesyncd 2>/dev/null || true\n")
	b.WriteString("else\n")
	b.WriteString("  echo \"bladerunner: chrony not active; leaving systemd-timesyncd in place\" >&2\n")
	b.WriteString("fi\n")

	// Watchdog port env file (the ONE templated piece — uses %d for the OIDC
	// local listener port the watchdog probes). VSOCK_OIDC_LOCAL_PORT is the
	// guest-side TCP listener (cfg.LocalOIDCPort); ssh :22 / incus :8443 are
	// fixed in-guest backends so the script hardcodes those.
	b.WriteString("\n# --- guest-local wake-heal watchdog: port env file (templated) ---\n")
	fmt.Fprintf(&b, "cat >/etc/default/bladerunner-watchdog <<EOF\nVSOCK_OIDC_LOCAL_PORT=%d\nEOF\n", cfg.LocalOIDCPort)

	// Watchdog script body (quoted heredoc; byte-for-byte the checked-in file).
	b.WriteString("cat >/usr/local/sbin/bladerunner-watchdog.sh <<'WATCHDOG'\n")
	b.WriteString(watchdogScript)
	b.WriteString("WATCHDOG\n")
	b.WriteString("chmod 0755 /usr/local/sbin/bladerunner-watchdog.sh\n")

	// Watchdog systemd unit (quoted heredoc).
	b.WriteString("cat >/etc/systemd/system/bladerunner-watchdog.service <<'WATCHDOGUNIT'\n")
	b.WriteString(watchdogUnit)
	b.WriteString("WATCHDOGUNIT\n")
	b.WriteString("systemctl daemon-reload\n")
	b.WriteString("systemctl enable --now bladerunner-watchdog.service || true\n")

	return b.String()
}

// renderShareSetup returns the guest-side bootstrap fragment that mounts the
// VirtioFS host<->guest share and pins ACPI poweroff so `br eject` triggers
// a deterministic clean shutdown. It returns "" when sharing is disabled
// (cfg.ShareDir == ""), so a non-cartridge boot emits no extra commands and is
// unchanged. The mount tag must match the host-side VirtioFS device tag
// (configureShare uses cfg.ShareTag, defaulting to config.DefaultShareTag).
//
// VirtioFS uid mapping: VZ mounts the share as the mounting context (root), so
// files default to root-owned. We chown the mountpoint to cfg.SSHUser so the
// default user can read+write the share. nofail keeps boot resilient if the
// share device is absent (e.g. a stripped image booted without a cartridge).
func renderShareSetup(cfg *config.Config) string {
	if cfg.ShareDir == "" {
		return ""
	}
	tag := cfg.ShareTag
	if tag == "" {
		tag = config.DefaultShareTag
	}
	guestPath := cfg.ShareGuestPath
	if guestPath == "" {
		guestPath = config.DefaultShareGuestPath
	}
	// The systemd .mount unit filename MUST be the escaped mount path
	// (/mnt/share -> mnt-share.mount) or systemd rejects it.
	unitName := strings.ReplaceAll(strings.TrimPrefix(guestPath, "/"), "/", "-") + ".mount"

	return fmt.Sprintf(`
# --- VirtioFS host<->guest share automount + ACPI poweroff pin (cartridge) ---
# The virtiofs kernel module is built in on Debian trixie genericcloud; load it
# best-effort for other images. nofail keeps boot resilient if the share device
# is absent (image booted without a cartridge).
modprobe virtiofs 2>/dev/null || true
mkdir -p %s

cat >/etc/systemd/system/%s <<'MOUNTUNIT'
[Unit]
Description=Bladerunner virtiofs host<->guest share
After=local-fs.target

[Mount]
What=%s
Where=%s
Type=virtiofs
Options=defaults,nofail,_netdev

[Install]
WantedBy=multi-user.target
MOUNTUNIT

# Belt-and-suspenders fstab line (same tag) so the share also mounts if the unit
# is ever masked; nofail so a missing device never blocks boot.
if ! grep -q '%s %s virtiofs' /etc/fstab 2>/dev/null; then
  echo '%s %s virtiofs defaults,nofail,_netdev 0 0' >> /etc/fstab
fi

systemctl daemon-reload
systemctl enable --now %s || true

# Make the share usable by the default SSH user (VirtioFS maps host files to the
# guest mounting context, i.e. root, so chown after the mount).
chown %s:%s %s 2>/dev/null || true

# Pin ACPI poweroff so the VZ ACPI power button (runner eject -> RequestStop)
# deterministically powers the guest off cleanly. Debian genericcloud + logind
# default to HandlePowerKey=poweroff already; make it explicit and robust.
mkdir -p /etc/systemd/logind.conf.d
cat >/etc/systemd/logind.conf.d/90-bladerunner.conf <<'LOGIND'
[Login]
HandlePowerKey=poweroff
HandlePowerKeyLongPress=poweroff
LOGIND
systemctl restart systemd-logind 2>/dev/null || true
`,
		guestPath,
		unitName,
		tag, guestPath,
		tag, guestPath,
		tag, guestPath,
		unitName,
		cfg.SSHUser, cfg.SSHUser, guestPath,
	)
}

func indent(s string, spaces int) string {
	prefix := strings.Repeat(" ", spaces)
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		if lines[i] == "" {
			lines[i] = prefix
			continue
		}
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n") + "\n"
}
