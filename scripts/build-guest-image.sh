#!/usr/bin/env bash
# scripts/build-guest-image.sh
#
# Build a pre-baked bladerunner guest image starting from the Debian Trixie
# genericcloud qcow2. The output image has incus + incus-ui-canonical, socat,
# jq pre-installed, and vsock kernel modules baked into the initramfs. It boots
# under the same cloud-init bladerunner emits, which no-ops the apt installs
# when the packages are already present.
#
# Usage:
#   scripts/build-guest-image.sh --arch arm64 --output bladerunner-guest-arm64.qcow2
#   scripts/build-guest-image.sh --arch amd64 --output bladerunner-guest-amd64.qcow2
#
# Required host tools (Debian/Ubuntu packages):
#   qemu-utils         (qemu-img)
#   libguestfs-tools   (guestfish, virt-customize, virt-sparsify)
#
# The script falls back to a qemu-nbd + chroot path only when libguestfs is
# unavailable; the libguestfs path is preferred because it is unprivileged
# inside a VM image and works on most GitHub-hosted runners without /dev/kvm.

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# Time-heal provisioning assets (chrony.conf + watchdog) live with the Go
# package that go:embed's them — internal/provision/scripts/ is the SINGLE SOURCE
# OF TRUTH shared by the cloud-init and image-build paths. The vsock relays
# (ssh/incus/oidc/ntp) are NOT baked here: after #160 every boot provisions via
# full cloud-init, which installs the templated bladerunner-vsock-relay@ unit +
# per-channel arg files fresh each boot, so baking them would be redundant.
ASSET_DIR="$(cd -- "${SCRIPT_DIR}/../internal/provision/scripts" && pwd)"
WORK_DIR="${WORK_DIR:-$(mktemp -d)}"
trap 'rm -rf "${WORK_DIR}"' EXIT

ARCH=""
OUTPUT=""
DEBIAN_RELEASE="${DEBIAN_RELEASE:-trixie}"
TARGET_SIZE_GIB="${TARGET_SIZE_GIB:-8}"
# Customize method: auto (prefer libguestfs), guestfish (force libguestfs), or
# nbd (force the qemu-nbd + chroot path). The chroot path runs apt over the
# HOST network namespace and never boots a libguestfs appliance, so it sidesteps
# the "passt exited with status 1" appliance-networking failure that libguestfs
# 1.52 hits on GitHub-hosted runners (#45).
METHOD="${GUEST_IMAGE_METHOD:-auto}"

log()   { printf '[build-guest-image] %s\n' "$*" >&2; }
fatal() { log "ERROR: $*"; exit 1; }

usage() {
    cat >&2 <<USAGE
usage: $0 --arch arm64|amd64 --output PATH

  --arch ARCH              Target architecture (arm64 or amd64).
  --output PATH            Output qcow2 path.
  --debian-release NAME    Override Debian release (default: trixie).
  --size GIB               Resize working image to this GiB (default: 8).
  --method METHOD          Customize method: auto|guestfish|nbd (default: auto).
                           'nbd' forces the qemu-nbd + chroot path, which avoids
                           the libguestfs appliance network (passt) failure on
                           GitHub-hosted runners.
USAGE
    exit 2
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --arch)              ARCH="${2:?--arch needs a value}"; shift 2;;
        --output)            OUTPUT="${2:?--output needs a value}"; shift 2;;
        --debian-release)    DEBIAN_RELEASE="${2:?}"; shift 2;;
        --size)              TARGET_SIZE_GIB="${2:?}"; shift 2;;
        --method)            METHOD="${2:?--method needs a value}"; shift 2;;
        -h|--help)           usage;;
        *)                   fatal "unknown argument: $1";;
    esac
done

[[ -z "${ARCH}"   ]] && usage
[[ -z "${OUTPUT}" ]] && usage

case "${ARCH}" in
    arm64|amd64) ;;
    *) fatal "unsupported arch: ${ARCH} (expected arm64 or amd64)";;
esac

mkdir -p "$(dirname -- "${OUTPUT}")"

# ----- tool checks --------------------------------------------------------

require_tool() {
    command -v "$1" >/dev/null 2>&1 || fatal "missing required tool: $1 (install $2)"
}

require_tool qemu-img    "qemu-utils"
require_tool curl        "curl"
require_tool sha256sum   "coreutils"

case "${METHOD}" in
    auto|guestfish|nbd) ;;
    *) fatal "unsupported --method: ${METHOD} (expected auto, guestfish, or nbd)";;
esac

have_guestfish=0
if command -v guestfish >/dev/null 2>&1 && command -v virt-customize >/dev/null 2>&1; then
    have_guestfish=1
fi

USE_GUESTFISH=0
case "${METHOD}" in
    guestfish)
        [[ ${have_guestfish} -eq 1 ]] || fatal "--method guestfish but libguestfs not installed"
        USE_GUESTFISH=1
        log "customize method: libguestfs (guestfish + virt-customize), forced"
        ;;
    nbd)
        log "customize method: qemu-nbd + chroot, forced (avoids libguestfs appliance passt failure)"
        require_tool qemu-nbd "qemu-utils"
        require_tool chroot   "coreutils"
        ;;
    auto)
        if [[ ${have_guestfish} -eq 1 ]]; then
            USE_GUESTFISH=1
            log "customize method: libguestfs (guestfish + virt-customize), auto-selected"
        else
            log "customize method: qemu-nbd + chroot (libguestfs not detected)"
            require_tool qemu-nbd "qemu-utils"
            require_tool chroot   "coreutils"
        fi
        ;;
esac

# ----- download base image ------------------------------------------------

UPSTREAM_URL="https://cloud.debian.org/images/cloud/${DEBIAN_RELEASE}/latest/debian-13-genericcloud-${ARCH}.qcow2"
BASE_IMAGE="${WORK_DIR}/base.qcow2"

log "downloading ${UPSTREAM_URL}"
curl --fail --location --silent --show-error --output "${BASE_IMAGE}" "${UPSTREAM_URL}"

# ----- resize working image ----------------------------------------------

log "resizing working image to ${TARGET_SIZE_GIB}G"
qemu-img resize "${BASE_IMAGE}" "${TARGET_SIZE_GIB}G"

# ----- customize ----------------------------------------------------------

INITRAMFS_MODULES='vmw_vsock_virtio_transport
vhost_vsock'

BUILD_DATE="$(date -u +%Y.%m.%d)"

if [[ ${USE_GUESTFISH} -eq 1 ]]; then
    # virt-customize wraps guestfish for declarative image edits. It is
    # idempotent across reruns and handles the initrd regeneration.
    CUSTOMIZE_ARGS=(
        # incus-ui-canonical is intentionally absent: it is not in Debian main
        # and apt-installing the Zabbly build would swap Debian's incus. The nbd
        # path bakes the UI as extracted static files; the libguestfs path keeps
        # the daemon only (dev convenience; CI uses --method nbd).
        --install "incus,incus-client,socat,jq,openssh-server,chrony"
        --run-command "systemctl enable incus incus.socket ssh"
        # chrony swap (suspend-tuned makestep) + guest-local wake-heal watchdog.
        # Single source of truth: internal/provision/scripts/{chrony.conf,bladerunner-watchdog.{sh,service}}.
        --copy-in     "${ASSET_DIR}/chrony.conf:/etc/chrony"
        --run-command "systemctl enable chrony"
        # Mask timesyncd once chrony is INSTALLED (half-removal guard). Offline
        # (virt-customize chroot) systemd is not running, so guard on the chronyd
        # binary, not 'is-active' which would always be false here.
        --run-command "command -v chronyd >/dev/null 2>&1 && (systemctl disable systemd-timesyncd || true; systemctl mask systemd-timesyncd || true) || true"
        --copy-in     "${ASSET_DIR}/bladerunner-watchdog.sh:/usr/local/sbin"
        --run-command "chmod 0755 /usr/local/sbin/bladerunner-watchdog.sh"
        --copy-in     "${ASSET_DIR}/bladerunner-watchdog.service:/etc/systemd/system"
        --run-command "systemctl enable bladerunner-watchdog.service"
        # The vsock relays (incl. the NTP bridge) are NOT baked: cloud-init
        # installs the templated bladerunner-vsock-relay@ unit + per-channel arg
        # files on every boot (post-#160), so baking them would only create a
        # stale duplicate the cloud-init run has to supersede.
        --append-line "/etc/initramfs-tools/modules:vmw_vsock_virtio_transport"
        --append-line "/etc/initramfs-tools/modules:vhost_vsock"
        --write       "/etc/bladerunner-image-version:${BUILD_DATE}"
        --run-command "update-initramfs -u"
    )

    log "running virt-customize"
    virt-customize -a "${BASE_IMAGE}" "${CUSTOMIZE_ARGS[@]}"
else
    # nbd + chroot fallback. Requires root and /dev/kvm-less qemu-nbd.
    NBD_DEV="${NBD_DEV:-/dev/nbd0}"
    MNT="${WORK_DIR}/mnt"
    mkdir -p "${MNT}"

    log "loading nbd module"
    modprobe nbd max_part=8

    log "attaching ${BASE_IMAGE} to ${NBD_DEV}"
    qemu-nbd --connect="${NBD_DEV}" "${BASE_IMAGE}"

    cleanup_nbd() {
        umount "${MNT}/dev" 2>/dev/null || true
        umount "${MNT}/proc" 2>/dev/null || true
        umount "${MNT}/sys" 2>/dev/null || true
        umount "${MNT}" 2>/dev/null || true
        qemu-nbd --disconnect "${NBD_DEV}" 2>/dev/null || true
    }
    trap 'cleanup_nbd; rm -rf "${WORK_DIR}"' EXIT

    sleep 2  # let kernel surface partitions
    log "partition layout of ${NBD_DEV}:"
    lsblk -o NAME,SIZE,FSTYPE,PARTLABEL "${NBD_DEV}" >&2 || true

    # Pick the root partition by filesystem rather than hardcoding p1: Debian
    # genericcloud carries a small FAT ESP and a BIOS-boot partition alongside
    # the ext4 root, and their ordering is not guaranteed across releases.
    ROOT_PART=""
    for part in "${NBD_DEV}"p*; do
        [[ -b "${part}" ]] || continue
        if [[ "$(blkid -o value -s TYPE "${part}" 2>/dev/null || true)" == "ext4" ]]; then
            ROOT_PART="${part}"
            break
        fi
    done
    [[ -n "${ROOT_PART}" ]] || ROOT_PART="${NBD_DEV}p1"  # last-resort fallback
    log "mounting root partition ${ROOT_PART}"
    mount "${ROOT_PART}" "${MNT}"
    mount --bind /dev  "${MNT}/dev"
    mount --bind /proc "${MNT}/proc"
    mount --bind /sys  "${MNT}/sys"

    # The Debian cloud image ships /etc/resolv.conf as a systemd-resolved symlink
    # that dangles inside the chroot, so apt can't resolve the mirror. The chroot
    # shares the host network namespace, so the host's resolver works here. Stash
    # the original and restore it afterwards so the baked image is unchanged.
    RESOLV="${MNT}/etc/resolv.conf"
    RESOLV_BAK="${WORK_DIR}/resolv.conf.orig"
    if [[ -e "${RESOLV}" || -L "${RESOLV}" ]]; then
        cp -a "${RESOLV}" "${RESOLV_BAK}"
    fi
    rm -f "${RESOLV}"
    cp -L /etc/resolv.conf "${RESOLV}" 2>/dev/null \
        || printf 'nameserver 1.1.1.1\nnameserver 8.8.8.8\n' > "${RESOLV}"

    # Watchdog script/unit copied into the chroot BEFORE it runs (their target
    # dirs already exist in the base image). chrony.conf is written AFTER apt
    # installs chrony inside the chroot (its /etc/chrony dir is created by the
    # package), via the single-source-of-truth file staged here.
    install -m 0755 "${ASSET_DIR}/bladerunner-watchdog.sh" "${MNT}/usr/local/sbin/bladerunner-watchdog.sh"
    install -m 0644 "${ASSET_DIR}/bladerunner-watchdog.service" "${MNT}/etc/systemd/system/bladerunner-watchdog.service"
    install -m 0644 "${ASSET_DIR}/chrony.conf" "${MNT}/root/bladerunner-chrony.conf"

    chroot "${MNT}" /bin/bash -eu <<EOS
export DEBIAN_FRONTEND=noninteractive
# Retry transient mirror/CDN resets (observed: a 'Connection reset by peer' on a
# single .deb fetch failing the whole amd64 build while arm64 succeeded).
echo 'Acquire::Retries "5";' > /etc/apt/apt.conf.d/80-bladerunner-retries
apt-get update
# Core packages from Debian trixie main. Do NOT apt-install incus-ui-canonical:
# it is not in main, and pulling it from Zabbly would swap Debian's incus to
# satisfy its "Depends: incus". The UI is baked below as static files instead.
apt-get install -y incus incus-client socat jq openssh-server chrony
systemctl enable incus incus.socket ssh
# Incus web UI (best-effort, matches the cloud-init path): download the Zabbly
# .deb and extract its static files to /opt/incus/ui WITHOUT installing the
# package, then point incusd at it via INCUS_UI. Entirely non-fatal so a missing
# Zabbly suite never fails the image build.
if [ ! -e /etc/apt/keyrings/zabbly.asc ]; then
  mkdir -p /etc/apt/keyrings
  curl -fsSL https://pkgs.zabbly.com/key.asc -o /etc/apt/keyrings/zabbly.asc || true
fi
cat >/etc/apt/sources.list.d/zabbly-incus-stable.sources <<SRC || true
Enabled: yes
Types: deb
URIs: https://pkgs.zabbly.com/incus/stable
Suites: trixie
Components: main
Architectures: \$(dpkg --print-architecture)
Signed-By: /etc/apt/keyrings/zabbly.asc
SRC
apt-get update || true
( cd /tmp && apt-get download incus-ui-canonical ) >/dev/null 2>&1 || true
UI_DEB=\$(ls -1 /tmp/incus-ui-canonical_*.deb 2>/dev/null | head -1)
if [ -n "\$UI_DEB" ]; then
  dpkg-deb -x "\$UI_DEB" / || true
  rm -f "\$UI_DEB"
  mkdir -p /etc/systemd/system/incus.service.d
  printf '[Service]\nEnvironment=INCUS_UI=/opt/incus/ui\n' >/etc/systemd/system/incus.service.d/10-bladerunner-ui.conf
  echo "baked Incus web UI to /opt/incus/ui"
else
  echo "incus-ui-canonical unavailable; web UI not baked (non-fatal)" >&2
fi
# Drop the Zabbly source so the baked image never carries it (UI files are
# already extracted; apt must never pull Zabbly's incus at runtime).
rm -f /etc/apt/sources.list.d/zabbly-incus-stable.sources
# chrony swap: install our suspend-tuned conf (overwriting the package default),
# enable chrony, then mask timesyncd ONLY if chrony is active (half-removal
# guard — never leave the guest with zero time sync).
install -m 0644 /root/bladerunner-chrony.conf /etc/chrony/chrony.conf
rm -f /root/bladerunner-chrony.conf
systemctl enable chrony
# Offline chroot: no running systemd, so guard the mask on chrony being INSTALLED
# (chronyd binary), not 'is-active' which is always false here.
if command -v chronyd >/dev/null 2>&1; then systemctl disable systemd-timesyncd || true; systemctl mask systemd-timesyncd || true; fi
systemctl enable bladerunner-watchdog.service
printf '%s\n' '${INITRAMFS_MODULES}' >> /etc/initramfs-tools/modules
printf '%s' '${BUILD_DATE}' > /etc/bladerunner-image-version
update-initramfs -u
# Drop the apt cache + build-time retry config so the baked image stays pristine.
apt-get clean
rm -rf /var/lib/apt/lists/*
rm -f /etc/apt/apt.conf.d/80-bladerunner-retries
EOS

    # Restore the image's original /etc/resolv.conf (typically the
    # systemd-resolved symlink) so the baked image resolves DNS at runtime the
    # same way stock Debian does.
    rm -f "${RESOLV}"
    if [[ -e "${RESOLV_BAK}" || -L "${RESOLV_BAK}" ]]; then
        cp -a "${RESOLV_BAK}" "${RESOLV}"
    fi

    # Zero the free space so qemu-img -c can compress it away: virt-sparsify
    # (which would discard unused blocks) can't run here because the libguestfs
    # appliance won't launch on the runner. Best-effort; ENOSPC just stops dd.
    log "zero-filling free space to aid compression"
    dd if=/dev/zero of="${MNT}/ZEROFILL" bs=1M 2>/dev/null || true
    rm -f "${MNT}/ZEROFILL"
    sync

    # Detach the image NOW (not just on the EXIT trap) so the compress step below
    # reads a consistent, fully-flushed qcow2 rather than one qemu-nbd still holds.
    cleanup_nbd
fi

# ----- sparsify and compress ----------------------------------------------

OUT_TMP="${WORK_DIR}/out.qcow2"
if [[ ${USE_GUESTFISH} -eq 1 ]] && command -v virt-sparsify >/dev/null 2>&1; then
    log "sparsifying with virt-sparsify"
    virt-sparsify --compress "${BASE_IMAGE}" "${OUT_TMP}"
else
    # nbd path (or no libguestfs): the appliance is unavailable, so compress with
    # qemu-img. Free space was zeroed above, so -c shrinks it effectively.
    log "compressing with qemu-img convert (virt-sparsify unavailable: no libguestfs appliance)"
    qemu-img convert -O qcow2 -c "${BASE_IMAGE}" "${OUT_TMP}"
fi

mv -- "${OUT_TMP}" "${OUTPUT}"

# ----- emit checksum ------------------------------------------------------

DIGEST="$(sha256sum -- "${OUTPUT}" | awk '{print $1}')"
log "built ${OUTPUT} (${DIGEST})"
printf '%s  %s\n' "${DIGEST}" "$(basename -- "${OUTPUT}")" > "${OUTPUT}.sha256"
printf '%s\n' "${DIGEST}"
