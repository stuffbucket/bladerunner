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
	if cfg.UseGuestAgent {
		return buildMinimalCloudInit(cfg)
	}
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

# --- vsock bridge units: create + enable BEFORE incus provisioning, so a
#     failure installing/configuring incus (or any later command) can never
#     strand host<->guest SSH. ConditionPathExists guards mean a unit simply
#     waits, rather than errors, if socat / its TCP target is not up yet.
cat >/etc/systemd/system/bladerunner-vsock-ssh.service <<'UNIT'
[Unit]
Description=Bladerunner vsock SSH forward
After=network.target ssh.service sshd.service
Wants=ssh.service sshd.service
ConditionPathExists=/usr/bin/socat
ConditionPathExists=/dev/vsock

[Service]
Type=simple
ExecStartPre=/bin/sh -c 'until ss -tln | grep -q ":22 "; do sleep 0.5; done'
ExecStart=/usr/bin/socat VSOCK-LISTEN:%d,fork,reuseaddr TCP:127.0.0.1:22
Restart=always
RestartSec=1

[Install]
WantedBy=multi-user.target
UNIT

cat >/etc/systemd/system/bladerunner-vsock-incus.service <<'UNIT'
[Unit]
Description=Bladerunner vsock Incus API forward
After=network.target incus.service incus.socket
Wants=incus.socket
ConditionPathExists=/usr/bin/socat
ConditionPathExists=/dev/vsock

[Service]
Type=simple
ExecStartPre=/bin/sh -c 'until ss -tln | grep -q ":8443 "; do sleep 0.5; done'
ExecStart=/usr/bin/socat VSOCK-LISTEN:%d,fork,reuseaddr TCP:127.0.0.1:8443
Restart=always
RestartSec=1

[Install]
WantedBy=multi-user.target
UNIT

cat >/etc/systemd/system/bladerunner-vsock-oidc.service <<'UNIT'
[Unit]
Description=Bladerunner vsock OIDC forward (guest TCP -> host vsock)
After=network.target
ConditionPathExists=/usr/bin/socat
ConditionPathExists=/dev/vsock

[Service]
Type=simple
ExecStart=/usr/bin/socat TCP-LISTEN:%d,bind=127.0.0.1,fork,reuseaddr VSOCK-CONNECT:2:%d
Restart=always
RestartSec=1

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable --now bladerunner-vsock-ssh.service || true
systemctl enable --now bladerunner-vsock-incus.service || true
systemctl enable --now bladerunner-vsock-oidc.service || true
%s%s
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

# incus should be listening now; nudge the API bridge so it picks up :8443
# without waiting for its restart timer.
systemctl restart bladerunner-vsock-incus.service || true

# Wait a moment for services to start
sleep 2

# Log vsock service status for debugging
echo "=== vsock diagnostics ===" | tee /var/lib/bladerunner/vsock-diag.txt
echo "--- /dev/vsock ---" | tee -a /var/lib/bladerunner/vsock-diag.txt
ls -la /dev/vsock 2>&1 | tee -a /var/lib/bladerunner/vsock-diag.txt || echo "not found"
echo "--- lsmod | grep vsock ---" | tee -a /var/lib/bladerunner/vsock-diag.txt
lsmod | grep vsock 2>&1 | tee -a /var/lib/bladerunner/vsock-diag.txt || echo "no modules loaded"
echo "--- bladerunner-vsock-ssh status ---" | tee -a /var/lib/bladerunner/vsock-diag.txt
systemctl status bladerunner-vsock-ssh.service --no-pager 2>&1 | tee -a /var/lib/bladerunner/vsock-diag.txt || true
echo "--- bladerunner-vsock-incus status ---" | tee -a /var/lib/bladerunner/vsock-diag.txt
systemctl status bladerunner-vsock-incus.service --no-pager 2>&1 | tee -a /var/lib/bladerunner/vsock-diag.txt || true

ip -4 -br addr show scope global > /var/lib/bladerunner/ipv4.txt || true
incus info > /var/lib/bladerunner/incus-info.txt || true
date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ >/var/lib/bladerunner/ready
br_stage bootstrap-done
`,
		// Break-glass SSH block (SSH_USER, SSH_PUBKEY), placed first because it
		// appears early in the bootstrap, before the vsock units.
		cfg.SSHUser, cfg.SSHPublicKey,
		cfg.VsockSSHPort, cfg.VsockAPIPort,
		// The guest OIDC TCP listener binds LocalOIDCPort (the issuer's port) so the
		// issuer URL resolves the same inside the guest as on the host; it then
		// connects over vsock to the host provider on VsockOIDCPort. (TCP-LISTEN
		// port = LocalOIDCPort, VSOCK-CONNECT port = VsockOIDCPort.)
		cfg.LocalOIDCPort, cfg.VsockOIDCPort,
		// chrony swap + guest-local wake-heal watchdog. Emitted before the share
		// fragment and before incus, so the time stack + backstop are in place
		// regardless of any later incus failure. Self-contained (its own Sprintf
		// for the port env file), so the positional arg list here is unchanged
		// apart from this single %s.
		renderTimeHeal(cfg),
		// VirtioFS share automount + ACPI poweroff pin, emitted only when a share
		// is enabled (empty otherwise, so plain start/boot is byte-identical).
		renderShareSetup(cfg),
		cfg.SSHUser,
		cfg.OIDCIssuerURL, cfg.OIDCClientID, cfg.OIDCAudience,
	)
}

// watchdogScript is the guest-local wake-heal watchdog body, kept BYTE-FOR-BYTE
// in sync with scripts/bladerunner-watchdog.sh (the single source of truth the
// image-build paths --copy-in). It is a backtick raw string so no fmt verbs are
// interpreted: every $ and % is literal. Do NOT introduce fmt.Sprintf verbs
// here — the port values are threaded via the /etc/default env file written by
// renderTimeHeal, NOT via substitution into this script.
const watchdogScript = `#!/usr/bin/env bash
# bladerunner-watchdog.sh — guest-LOCAL wake-heal backstop.
#
# SINGLE SOURCE OF TRUTH: this file is checked in and --copy-in'd by the image
# build (scripts/build-guest-image.sh). The cloud-init path embeds a byte-for-
# byte copy of this body in internal/provision/cloudinit.go; keep them in sync.
#
# WHY THIS EXISTS: when the Mac sleeps, VZ pauses the guest vCPUs with no clean
# ACPI suspend and no paravirt "you were stopped" signal. On wake the guest has
# NO event telling it that it was suspended. A watchdog CANNOT detect "just woke"
# by comparing its own monotonic vs wall clock — both freeze and resume together
# (arch_sys_counter drives both; no kvm-clock, no /dev/ptp). So the only
# guest-local skew detector is an EXTERNAL reference: chrony's NTP offset (and
# the RTC, IF it tracks real time — UNKNOWN on VZ, so we only LOG it).
#
# CONFIRMED: stale vsock connectivity (no SSH banner) is the more certain
# symptom. PLAUSIBLE-but-UNCONFIRMED: post-sleep clock skew breaking OIDC JWTs
# (the wedged VM was restarted before its clock could be measured). This loop
# LOGS both signals EVERY cycle so the NEXT wedge is DIAGNOSED, not guessed.
#
# Heal is conservative: never blanket-restart systemd-networkd (it would disrupt
# running Incus container bridges); only bounce the stateless socat relays, and
# only bounce vsock-ssh under a tight gate (sshd up locally but the relay dead).
set -uo pipefail
[ -r /etc/default/bladerunner-watchdog ] && . /etc/default/bladerunner-watchdog
VSOCK_OIDC_LOCAL_PORT="${VSOCK_OIDC_LOCAL_PORT:-15556}"  # the guest TCP listener
TAG=bladerunner-watchdog

log() { logger -t "$TAG" -- "$*"; }

listening() { ss -tln | grep -q ":$1 "; }
unit_active() { systemctl is-active --quiet "$1"; }
unit_restarts() { systemctl show -p NRestarts --value "$1" 2>/dev/null; }

while :; do
  # --- OBSERVE: clock (chrony) -------------------------------------------
  # System time offset + Leap status are the external skew reference.
  tracking="$(chronyc tracking 2>/dev/null)"
  sys_offset="$(printf '%s\n' "$tracking" | awk -F': ' '/System time/ {print $2}')"
  leap="$(printf '%s\n' "$tracking" | awk -F': ' '/Leap status/ {print $2}')"
  refid="$(printf '%s\n' "$tracking" | awk -F': ' '/Reference ID/ {print $2}')"
  # --- OBSERVE: RTC-vs-realtime delta (the empirical test of the UNKNOWN:
  #     does the VZ RTC advance during host sleep? If yes, a future wedge
  #     will show a big spike here. We only LOG it; we do NOT heal from it
  #     until a real sleep confirms the RTC tracks real time.) ------------
  rtc_epoch="$(cat /sys/class/rtc/rtc0/since_epoch 2>/dev/null || echo NA)"
  now_epoch="$(date +%s)"
  if [ "$rtc_epoch" != NA ]; then rtc_delta=$((rtc_epoch - now_epoch)); else rtc_delta=NA; fi
  # --- OBSERVE: per-forwarder local health -------------------------------
  ssh_listen=$(listening 22 && echo up || echo down)
  api_listen=$(listening 8443 && echo up || echo down)
  oidc_listen=$(listening "$VSOCK_OIDC_LOCAL_PORT" && echo up || echo down)
  for u in bladerunner-vsock-ssh bladerunner-vsock-incus bladerunner-vsock-oidc bladerunner-vsock-ntp; do
    st=$(unit_active "$u.service" && echo active || echo inactive)
    log "fwd $u=$st nrestarts=$(unit_restarts "$u.service")"
  done
  log "clock sys_offset=${sys_offset:-NA} leap=${leap:-NA} refid=${refid:-NA} rtc_delta=${rtc_delta}s"
  log "listen ssh22=$ssh_listen api8443=$api_listen oidc${VSOCK_OIDC_LOCAL_PORT}=$oidc_listen"

  # --- HEAL: clock. Don't wait for chrony's autonomous poll (up to maxpoll
  #     ~64s away) to notice a post-sleep offset: actively force a fresh host
  #     measurement NOW (burst). chrony.conf's 'makestep 1.0 -1' then steps the
  #     clock on that measurement for any offset >1s (and slews smaller ones),
  #     so re-sync is bounded by THIS loop's period, not loop + a chrony poll.
  #     A step never disturbs CLOCK_MONOTONIC, so timers/containers are fine.
  #     The explicit makestep is a backstop for when chrony has already parked
  #     the source as unreachable (its persistent makestep won't re-fire then).
  chronyc burst 4/4 >/dev/null 2>&1 || true
  if [ "$leap" = "Not synchronised" ]; then
    log "heal: chronyc makestep (leap=Not synchronised)"
    chronyc makestep >/dev/null 2>&1 || true
  fi

  # --- HEAL: stateless socat relays (incus, then oidc). Restarting these
  #     only drops in-flight proxied connections; container/network state is
  #     owned by incusd + systemd-networkd, never by the relay. Bounce only
  #     when the relay is dead/inactive (its backend being down is the
  #     target's problem, not the relay's — do NOT bounce then).
  if [ "$api_listen" = up ] && ! unit_active bladerunner-vsock-incus.service; then
    log "heal: restart bladerunner-vsock-incus (backend :8443 up, relay inactive)"
    systemctl restart bladerunner-vsock-incus.service || true
  fi
  if ! unit_active bladerunner-vsock-oidc.service; then
    log "heal: restart bladerunner-vsock-oidc (relay inactive)"
    systemctl restart bladerunner-vsock-oidc.service || true
  fi
  if ! unit_active bladerunner-vsock-ntp.service; then
    log "heal: restart bladerunner-vsock-ntp (relay inactive)"
    systemctl restart bladerunner-vsock-ntp.service || true
  fi

  # --- HEAL: vsock-ssh LAST and tightly gated. Only when local sshd IS
  #     listening (:22 up, so the in-guest target is healthy) but the relay
  #     is dead. This avoids racing an operator's live 'br shell' and
  #     avoids spinning the unit's unbounded ExecStartPre when sshd is down.
  #     The watchdog runs locally, so bouncing the bridge never cuts its own
  #     execution path. (Restart=always already auto-heals a crashed socat,
  #     so this fires only for the rare wedged-but-not-crashed case.)
  if [ "$ssh_listen" = up ] && ! unit_active bladerunner-vsock-ssh.service; then
    log "heal: restart bladerunner-vsock-ssh (sshd :22 up, relay inactive)"
    systemctl restart bladerunner-vsock-ssh.service || true
  fi

  # NEVER blanket-restart systemd-networkd: it would tear down Incus container
  # bridges. (A genuine total-network-down recovery is intentionally out of
  # scope until the journal data above proves it is needed.)
  #
  # OIDC probe honesty: ":$VSOCK_OIDC_LOCAL_PORT listening" proves only that the
  # guest-side socat TCP listener is up; it CANNOT confirm the host vsock peer
  # answers (the host is unreachable-by-design from a guest-local probe). The log
  # therefore distinguishes "relay dead" from "relay up, host unknown" — it does
  # NOT infer the host is fine.

  sleep 60
done
`

// watchdogUnit is the systemd unit for the watchdog, kept in sync with
// scripts/bladerunner-watchdog.service. Raw string: no fmt verbs.
const watchdogUnit = `[Unit]
Description=Bladerunner guest-local wake-heal watchdog
After=network.target chrony.service
Wants=chrony.service

[Service]
Type=simple
ExecStart=/usr/local/sbin/bladerunner-watchdog.sh
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`

// chronyConf is the suspend-tuned chrony.conf written to /etc/chrony/chrony.conf
// in the cloud-init path, kept in sync with scripts/chrony.conf. Raw string with
// no fmt verbs (it contains no % so it is safe verbatim). makestep 1.0 -1 steps
// the clock for ANY offset >1s an UNLIMITED number of times — the guest-local
// recovery for a host suspend (no paravirt "you were stopped" signal exists).
const chronyConf = `# Host pseudo-NTP source over vsock (guest UDP 123 -> bladerunner-vsock-ntp.service
# -> vsock -> host SNTP responder serving the HOST clock as stratum-1). The guest
# coheres to the HOST clock (not UTC) and works fully OFFLINE: vsock needs no IP
# network, no gateway, no host internet. The public pool is intentionally DROPPED
# — it requires internet (fails in airplane mode) and syncs to UTC, not the host.
# Poll lives on this line: maxpoll 6 (~64s) is tighter than the old global 8
# (~256s) so a post-sleep offset is noticed sooner. iburst makes the FIRST sync
# after resume happen within seconds.
server 127.0.0.1 iburst prefer minpoll 4 maxpoll 6
# makestep 1.0 -1 = step the clock for ANY offset > 1s, an UNLIMITED number of
# times (the "-1" count). This is the guest-LOCAL recovery for a host suspend:
# the guest gets no paravirt "you were stopped" signal, so chrony stepping on
# its first post-resume measurement is the only automatic local fix. The common
# default "makestep 1.0 3" only steps the first 3 updates and would SLEW a
# post-sleep jump over many minutes, during which OIDC JWT iat/exp/nbf stay
# broken. CONFIRMED: a large post-sleep offset needs an immediate step.
# UNCONFIRMED: the exact magnitude (needs a real Mac-sleep measurement).
makestep 1.0 -1
# Keep the RTC disciplined from system time so a future cold boot starts close.
rtcsync
driftfile /var/lib/chrony/chrony.drift
logdir /var/log/chrony
`

// vsockNTPUnit renders the bladerunner-vsock-ntp.service unit body. It is the
// single source for that unit: the cloud-init path heredocs the return value,
// and the image-build path --copy-ins the checked-in
// scripts/bladerunner-vsock-ntp.service. The two must stay byte-identical; the
// only difference is the templated port, whose default (config.DefaultVsockNTPPort)
// is the literal baked into that file. TestProvisioningAssetsMatchCheckedInFiles
// enforces the byte-identity against the checked-in copy.
func vsockNTPUnit(port uint32) string {
	return fmt.Sprintf("[Unit]\nDescription=Bladerunner vsock NTP forward (guest UDP 123 -> host vsock)\n"+
		"After=network.target\nConditionPathExists=/usr/bin/socat\nConditionPathExists=/dev/vsock\n\n"+
		"[Service]\nType=simple\n"+
		"ExecStart=/usr/bin/socat UDP4-RECVFROM:123,bind=127.0.0.1,fork,reuseaddr VSOCK-CONNECT:2:%d\n"+
		"Restart=always\nRestartSec=1\n\n"+
		"[Install]\nWantedBy=multi-user.target\n", port)
}

// renderTimeHeal returns the guest-side bootstrap fragment that (1) swaps
// systemd-timesyncd for chrony tuned to step the clock immediately after a host
// suspend, and (2) installs the guest-local wake-heal watchdog (script + unit).
// It is always emitted (no host/agent dependency). The chrony swap is guarded:
// timesyncd is only disabled+masked AFTER chrony is verified active, so a
// transient chrony install failure never strands the guest with zero time sync.
//
// The watchdog script body + unit are byte-for-byte copies of the checked-in
// scripts/bladerunner-watchdog.{sh,service}; the image-build paths --copy-in
// those same files. The only templated values are the port(s), threaded via the
// /etc/default/bladerunner-watchdog env file (so the script body needs no
// substitution and stays identical to the checked-in copy).
func renderTimeHeal(cfg *config.Config) string {
	var b strings.Builder

	// vsock NTP bridge: guest localhost UDP 123 -> vsock -> host SNTP responder.
	// UDP4-RECVFROM,fork = one vsock stream per datagram (48 in / 48 out). The
	// guest chrony "server 127.0.0.1" line in chronyConf targets this bridge. The
	// bridge is emitted BEFORE the chrony.conf write so the transport exists the
	// moment chrony enables.
	//
	// DUAL-SOURCE DISCIPLINE: the unit body lives in vsockNTPUnit (the single
	// source), which must stay BYTE-IDENTICAL to the checked-in
	// scripts/bladerunner-vsock-ntp.service (which the image-build arms --copy-in)
	// except for the templated port, whose default IS the 18557 hardcoded in that
	// file. TestProvisioningAssetsMatchCheckedInFiles guards the byte-identity —
	// same discipline as chronyConf above.
	b.WriteString("\n# --- vsock NTP bridge: guest UDP 123 -> vsock -> host SNTP responder ---\n")
	b.WriteString("cat >/etc/systemd/system/bladerunner-vsock-ntp.service <<'UNIT'\n")
	b.WriteString(vsockNTPUnit(cfg.VsockNTPPort))
	b.WriteString("UNIT\n")
	b.WriteString("systemctl daemon-reload\n")
	b.WriteString("systemctl enable --now bladerunner-vsock-ntp.service || true\n")

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

// buildMinimalCloudInit produces the five-line user-data used when the
// in-guest br-agent will take over configuration once the VM is up. The
// agent must already be present in the guest image (#45) or installed by
// the user via image bake; cloud-init merely seeds the SSH key and starts
// the agent.
//
// This path provisions NOTHING time-related (no chrony, no watchdog): it relies
// entirely on the pre-baked image (scripts/build-guest-image.sh, paths B/C) to
// carry chrony + the guest-local wake-heal watchdog. If you change the chrony /
// watchdog provisioning, the baked image MUST be rebuilt or this agent path
// regresses to systemd-timesyncd with no wake-heal backstop.
func buildMinimalCloudInit(cfg *config.Config) (string, string) {
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	fmt.Fprintf(&b, "hostname: %s\n", cfg.Hostname)
	b.WriteString("manage_etc_hosts: true\n")
	b.WriteString("users:\n")
	b.WriteString("  - default\n")
	fmt.Fprintf(&b, "  - name: %s\n", cfg.SSHUser)
	b.WriteString("    shell: /bin/bash\n")
	b.WriteString("    sudo: ALL=(ALL) NOPASSWD:ALL\n")
	b.WriteString("    groups: [sudo]\n")
	b.WriteString("    lock_passwd: true\n")
	b.WriteString("    ssh_authorized_keys:\n")
	fmt.Fprintf(&b, "      - %s\n", cfg.SSHPublicKey)
	// The pre-baked image ships incus + br-agent but, unlike the full cloud-init
	// path, no host<->guest vsock bridges — and the in-guest br-agent does not
	// create them either. Without these socat relays the host can reach neither
	// SSH (vsock %[1]d -> :22) nor the Incus API (vsock %[2]d -> :8443), so the
	// guest looks dead even though it booted (#45). Emit the same bridge units
	// the full path uses (ports templated to cfg) and enable them before the
	// agent configures incus.
	b.WriteString("runcmd:\n")
	// Explicitly create the SSH user + authorized_keys. cloud-init's users module
	// is unreliable on this image (the agent observed "unknown user incus" and
	// key-based SSH was rejected), so mirror the full path's runcmd break-glass to
	// guarantee a key-based way in independent of the users module and the agent.
	b.WriteString("  - |\n")
	fmt.Fprintf(&b, "    SSH_USER='%s'\n", cfg.SSHUser)
	fmt.Fprintf(&b, "    SSH_PUBKEY='%s'\n", cfg.SSHPublicKey)
	b.WriteString("    if ! id -u \"$SSH_USER\" >/dev/null 2>&1; then useradd -m -s /bin/bash \"$SSH_USER\" || true; fi\n")
	b.WriteString("    usermod -s /bin/bash \"$SSH_USER\" 2>/dev/null || true\n")
	b.WriteString("    [ -d \"/home/$SSH_USER\" ] || mkdir -p \"/home/$SSH_USER\"\n")
	b.WriteString("    mkdir -p \"/home/$SSH_USER/.ssh\"\n")
	b.WriteString("    printf '%s\\n' \"$SSH_PUBKEY\" > \"/home/$SSH_USER/.ssh/authorized_keys\"\n")
	b.WriteString("    chmod 700 \"/home/$SSH_USER/.ssh\"\n")
	b.WriteString("    chmod 600 \"/home/$SSH_USER/.ssh/authorized_keys\"\n")
	b.WriteString("    chown -R \"$SSH_USER:$SSH_USER\" \"/home/$SSH_USER\" 2>/dev/null || true\n")
	b.WriteString("    usermod -aG sudo \"$SSH_USER\" 2>/dev/null || true\n")
	b.WriteString("    echo \"$SSH_USER ALL=(ALL) NOPASSWD:ALL\" > /etc/sudoers.d/90-bladerunner\n")
	b.WriteString("    chmod 440 /etc/sudoers.d/90-bladerunner\n")
	b.WriteString("    systemctl restart ssh 2>/dev/null || systemctl restart sshd 2>/dev/null || true\n")
	b.WriteString("  - |\n")
	b.WriteString("    cat >/etc/systemd/system/bladerunner-vsock-ssh.service <<'UNIT'\n")
	b.WriteString("    [Unit]\n")
	b.WriteString("    Description=Bladerunner vsock SSH forward\n")
	b.WriteString("    After=network.target ssh.service sshd.service\n")
	b.WriteString("    Wants=ssh.service sshd.service\n")
	b.WriteString("    ConditionPathExists=/usr/bin/socat\n")
	b.WriteString("    ConditionPathExists=/dev/vsock\n")
	b.WriteString("\n")
	b.WriteString("    [Service]\n")
	b.WriteString("    Type=simple\n")
	b.WriteString("    ExecStartPre=/bin/sh -c 'until ss -tln | grep -q \":22 \"; do sleep 0.5; done'\n")
	fmt.Fprintf(&b, "    ExecStart=/usr/bin/socat VSOCK-LISTEN:%d,fork,reuseaddr TCP:127.0.0.1:22\n", cfg.VsockSSHPort)
	b.WriteString("    Restart=always\n")
	b.WriteString("    RestartSec=1\n")
	b.WriteString("\n")
	b.WriteString("    [Install]\n")
	b.WriteString("    WantedBy=multi-user.target\n")
	b.WriteString("    UNIT\n")
	b.WriteString("    cat >/etc/systemd/system/bladerunner-vsock-incus.service <<'UNIT'\n")
	b.WriteString("    [Unit]\n")
	b.WriteString("    Description=Bladerunner vsock Incus API forward\n")
	b.WriteString("    After=network.target incus.service incus.socket\n")
	b.WriteString("    Wants=incus.socket\n")
	b.WriteString("    ConditionPathExists=/usr/bin/socat\n")
	b.WriteString("    ConditionPathExists=/dev/vsock\n")
	b.WriteString("\n")
	b.WriteString("    [Service]\n")
	b.WriteString("    Type=simple\n")
	b.WriteString("    ExecStartPre=/bin/sh -c 'until ss -tln | grep -q \":8443 \"; do sleep 0.5; done'\n")
	fmt.Fprintf(&b, "    ExecStart=/usr/bin/socat VSOCK-LISTEN:%d,fork,reuseaddr TCP:127.0.0.1:8443\n", cfg.VsockAPIPort)
	b.WriteString("    Restart=always\n")
	b.WriteString("    RestartSec=1\n")
	b.WriteString("\n")
	b.WriteString("    [Install]\n")
	b.WriteString("    WantedBy=multi-user.target\n")
	b.WriteString("    UNIT\n")
	b.WriteString("    systemctl daemon-reload\n")
	b.WriteString("    systemctl enable --now bladerunner-vsock-ssh.service bladerunner-vsock-incus.service\n")
	b.WriteString("  - [systemctl, enable, --now, br-agent.service]\n")

	metaData := fmt.Sprintf("instance-id: bladerunner-%s\nlocal-hostname: %s\n", cfg.Name, cfg.Hostname)
	return b.String(), metaData
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
