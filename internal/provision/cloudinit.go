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
)

func BuildCloudInit(cfg *config.Config, clientCertPEM string) (string, string) {
	bootstrapScript := renderBootstrapScript(cfg)

	var b strings.Builder
	b.WriteString("#cloud-config\n")
	b.WriteString(fmt.Sprintf("hostname: %s\n", cfg.Hostname))
	b.WriteString("manage_etc_hosts: true\n")
	aptMirror := config.DefaultAptMirrorURI(cfg.Arch)
	b.WriteString("apt:\n")
	b.WriteString("  primary:\n")
	b.WriteString("    - arches: [default]\n")
	b.WriteString(fmt.Sprintf("      uri: %s\n", aptMirror))
	b.WriteString("  security:\n")
	b.WriteString("    - arches: [default]\n")
	b.WriteString(fmt.Sprintf("      uri: %s\n", aptMirror))
	b.WriteString("users:\n")
	b.WriteString("  - default\n")
	b.WriteString(fmt.Sprintf("  - name: %s\n", cfg.SSHUser))
	b.WriteString("    shell: /bin/bash\n")
	b.WriteString("    sudo: ALL=(ALL) NOPASSWD:ALL\n")
	b.WriteString("    groups: [sudo]\n")
	b.WriteString("    lock_passwd: false\n")
	b.WriteString("    ssh_authorized_keys:\n")
	b.WriteString(fmt.Sprintf("      - %s\n", cfg.SSHPublicKey))
	b.WriteString("chpasswd:\n")
	b.WriteString("  expire: false\n")
	b.WriteString("  users:\n")
	b.WriteString(fmt.Sprintf("    - name: %s\n", cfg.SSHUser))
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
	b.WriteString("bootcmd:\n")
	b.WriteString("  # Enable serial console for boot messages\n")
	b.WriteString("  - |\n")
	b.WriteString("    if grep -q '^GRUB_CMDLINE_LINUX=' /etc/default/grub && ! grep -q 'console=' /etc/default/grub; then\n")
	b.WriteString("      sed -i 's/^GRUB_CMDLINE_LINUX=\"\\(.*\\)\"/GRUB_CMDLINE_LINUX=\"\\1 console=hvc0 console=tty0\"/' /etc/default/grub\n")
	b.WriteString("      update-grub || grub-mkconfig -o /boot/grub/grub.cfg || true\n")
	b.WriteString("    fi\n")
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
		if fileExists(c) {
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

# Load vsock kernel modules early (required for socat VSOCK-LISTEN)
# These may be built-in on some kernels, so ignore errors.
modprobe vsock 2>/dev/null || true
modprobe vmw_vsock_virtio_transport 2>/dev/null || true
modprobe vhost_vsock 2>/dev/null || true

# Verify vsock is available
if [ ! -e /dev/vsock ]; then
  echo "WARNING: /dev/vsock not found, vsock forwarding may not work" >&2
fi

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

if command -v apt-get >/dev/null 2>&1; then
  apt-get update -qq
  apt-get install -y -qq ca-certificates curl gpg openssh-server socat jq
  if ! apt-get install -y -qq incus incus-client; then
    install_zabbly_repo
    apt-get update -qq
    apt-get install -y -qq incus incus-client
  fi
elif command -v dnf >/dev/null 2>&1; then
  dnf install -y -q openssh-server socat incus incus-client || true
fi

if getent group incus-admin >/dev/null 2>&1; then
  usermod -a -G incus-admin %s || true
fi

systemctl enable --now ssh || true
systemctl enable --now sshd || true
systemctl enable --now incus || true
systemctl enable --now incus.socket || true

for i in $(seq 1 60); do
  if incus admin waitready --timeout=1 >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

incus admin init --auto || true
incus config set core.https_address "[::]:8443" || true

# Add the host client certificate to trust store (try multiple command variants)
incus config trust add-certificate /var/lib/bladerunner/host-client.crt --name bladerunner-host 2>/dev/null ||
  incus config trust add /var/lib/bladerunner/host-client.crt --name bladerunner-host 2>/dev/null ||
  echo "Note: Could not add host certificate to trust store (may already exist)"

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

systemctl daemon-reload
systemctl enable --now bladerunner-vsock-ssh.service
systemctl enable --now bladerunner-vsock-incus.service

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
`, cfg.SSHUser, cfg.VsockSSHPort, cfg.VsockAPIPort)
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

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}
