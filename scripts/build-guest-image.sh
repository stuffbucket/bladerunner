#!/usr/bin/env bash
# scripts/build-guest-image.sh
#
# Build a pre-baked bladerunner guest image starting from the Debian Trixie
# genericcloud qcow2. The output image has incus + incus-ui-canonical, socat,
# jq pre-installed, vsock kernel modules baked into the initramfs, and (when
# --br-agent-binary is supplied) the br-agent systemd unit ready to run on
# first boot.
#
# Usage:
#   scripts/build-guest-image.sh --arch arm64 --output bladerunner-guest-arm64.qcow2
#   scripts/build-guest-image.sh --arch amd64 --output bladerunner-guest-amd64.qcow2 \
#       --br-agent-binary ./bin/br-agent-amd64
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
WORK_DIR="${WORK_DIR:-$(mktemp -d)}"
trap 'rm -rf "${WORK_DIR}"' EXIT

ARCH=""
OUTPUT=""
BR_AGENT_BINARY=""
PACKAGES_DIR=""
DEBIAN_RELEASE="${DEBIAN_RELEASE:-trixie}"
TARGET_SIZE_GIB="${TARGET_SIZE_GIB:-8}"

log()   { printf '[build-guest-image] %s\n' "$*" >&2; }
fatal() { log "ERROR: $*"; exit 1; }

usage() {
    cat >&2 <<USAGE
usage: $0 --arch arm64|amd64 --output PATH [--br-agent-binary PATH]

  --arch ARCH              Target architecture (arm64 or amd64).
  --output PATH            Output qcow2 path.
  --br-agent-binary PATH   Optional br-agent binary; if absent the image is
                           built without the agent (suffix the artifact name
                           with -no-agent at the caller).
  --packages-dir DIR       Optional dir of pre-downloaded .deb files. When set,
                           the libguestfs path installs these OFFLINE (copy-in +
                           dpkg) instead of running apt inside the appliance.
                           This avoids appliance networking entirely, so the
                           libguestfs `passt`/SLIRP network helper never runs —
                           the reliable path on GitHub-hosted runners. The dir
                           must contain the full dependency closure of
                           incus, incus-ui-canonical, socat, jq, openssh-server,
                           chrony (e.g. produced by `apt-get download` of the
                           closure inside a debian:${DEBIAN_RELEASE} container).
  --debian-release NAME    Override Debian release (default: trixie).
  --size GIB               Resize working image to this GiB (default: 8).
USAGE
    exit 2
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --arch)              ARCH="${2:?--arch needs a value}"; shift 2;;
        --output)            OUTPUT="${2:?--output needs a value}"; shift 2;;
        --br-agent-binary)   BR_AGENT_BINARY="${2:?--br-agent-binary needs a value}"; shift 2;;
        --packages-dir)      PACKAGES_DIR="${2:?--packages-dir needs a value}"; shift 2;;
        --debian-release)    DEBIAN_RELEASE="${2:?}"; shift 2;;
        --size)              TARGET_SIZE_GIB="${2:?}"; shift 2;;
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

USE_GUESTFISH=0
if command -v guestfish >/dev/null 2>&1 && command -v virt-customize >/dev/null 2>&1; then
    USE_GUESTFISH=1
    log "using libguestfs (guestfish + virt-customize)"
else
    log "libguestfs not detected; falling back to qemu-nbd + chroot path"
    require_tool qemu-nbd "qemu-utils"
    require_tool chroot   "coreutils"
fi

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

# Install br-agent + systemd unit (no-op when binary is missing).
if [[ -n "${BR_AGENT_BINARY}" ]]; then
    [[ -f "${BR_AGENT_BINARY}" ]] || fatal "br-agent binary not found: ${BR_AGENT_BINARY}"
    log "br-agent binary: ${BR_AGENT_BINARY}"
fi

if [[ ${USE_GUESTFISH} -eq 1 ]]; then
    # virt-customize wraps guestfish for declarative image edits. It is
    # idempotent across reruns and handles the initrd regeneration.
    #
    # Package install has two modes:
    #   - default: --install pulls packages with apt INSIDE the appliance,
    #     which requires appliance networking (the libguestfs passt/SLIRP
    #     helper). That helper is unreliable on GitHub-hosted runners.
    #   - --packages-dir: copy pre-downloaded .debs into the appliance and
    #     `dpkg -i` them OFFLINE, so virt-customize never enables networking
    #     and passt never runs. Preferred in CI.
    CUSTOMIZE_ARGS=()
    if [[ -n "${PACKAGES_DIR}" ]]; then
        [[ -d "${PACKAGES_DIR}" ]] || fatal "packages dir not found: ${PACKAGES_DIR}"
        shopt -s nullglob
        debs=("${PACKAGES_DIR}"/*.deb)
        shopt -u nullglob
        [[ ${#debs[@]} -gt 0 ]] || fatal "no .deb files in ${PACKAGES_DIR}"
        log "offline install: ${#debs[@]} .debs from ${PACKAGES_DIR} (no appliance network)"
        CUSTOMIZE_ARGS+=(
            --copy-in     "${PACKAGES_DIR}:/tmp"
            # dpkg -i is order-sensitive; a second pass resolves install order
            # within the closure. The set is self-contained (full dep closure),
            # so no apt/network is needed to satisfy dependencies.
            --run-command "dpkg -i /tmp/$(basename "${PACKAGES_DIR}")/*.deb || dpkg -i /tmp/$(basename "${PACKAGES_DIR}")/*.deb"
            --run-command "rm -rf /tmp/$(basename "${PACKAGES_DIR}")"
        )
    else
        CUSTOMIZE_ARGS+=(
            --install "incus,incus-ui-canonical,socat,jq,openssh-server,chrony"
        )
    fi
    CUSTOMIZE_ARGS+=(
        --run-command "systemctl enable incus incus.socket ssh"
        # chrony swap (suspend-tuned makestep) + guest-local wake-heal watchdog.
        # Single source of truth: scripts/chrony.conf + scripts/bladerunner-watchdog.{sh,service}.
        --copy-in     "${SCRIPT_DIR}/chrony.conf:/etc/chrony"
        --run-command "systemctl enable chrony"
        # Mask timesyncd once chrony is INSTALLED (half-removal guard). Offline
        # (virt-customize chroot) systemd is not running, so guard on the chronyd
        # binary, not 'is-active' which would always be false here.
        --run-command "command -v chronyd >/dev/null 2>&1 && (systemctl disable systemd-timesyncd || true; systemctl mask systemd-timesyncd || true) || true"
        --copy-in     "${SCRIPT_DIR}/bladerunner-watchdog.sh:/usr/local/sbin"
        --run-command "chmod 0755 /usr/local/sbin/bladerunner-watchdog.sh"
        --copy-in     "${SCRIPT_DIR}/bladerunner-watchdog.service:/etc/systemd/system"
        --run-command "systemctl enable bladerunner-watchdog.service"
        # vsock NTP bridge: guest UDP 123 -> vsock -> host SNTP responder. Baked
        # into the image so the agent/guest-agent path (which emits no time-stack
        # cloud-init) still has the bridge. Single source: scripts/bladerunner-vsock-ntp.service.
        --copy-in     "${SCRIPT_DIR}/bladerunner-vsock-ntp.service:/etc/systemd/system"
        --run-command "systemctl enable bladerunner-vsock-ntp.service"
        --append-line "/etc/initramfs-tools/modules:vmw_vsock_virtio_transport"
        --append-line "/etc/initramfs-tools/modules:vhost_vsock"
        --write       "/etc/bladerunner-image-version:${BUILD_DATE}"
        --run-command "update-initramfs -u"
    )

    if [[ -n "${BR_AGENT_BINARY}" ]]; then
        CUSTOMIZE_ARGS+=(
            --copy-in   "${BR_AGENT_BINARY}:/usr/local/bin"
            --run-command "mv /usr/local/bin/$(basename "${BR_AGENT_BINARY}") /usr/local/bin/br-agent && chmod +x /usr/local/bin/br-agent"
            --copy-in   "${SCRIPT_DIR}/br-agent.service:/etc/systemd/system"
            --run-command "systemctl enable br-agent.service"
        )
    fi

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
    ROOT_PART="${NBD_DEV}p1"
    mount "${ROOT_PART}" "${MNT}"
    mount --bind /dev  "${MNT}/dev"
    mount --bind /proc "${MNT}/proc"
    mount --bind /sys  "${MNT}/sys"

    # Watchdog script/unit copied into the chroot BEFORE it runs (their target
    # dirs already exist in the base image). chrony.conf is written AFTER apt
    # installs chrony inside the chroot (its /etc/chrony dir is created by the
    # package), via the single-source-of-truth file staged here.
    install -m 0755 "${SCRIPT_DIR}/bladerunner-watchdog.sh" "${MNT}/usr/local/sbin/bladerunner-watchdog.sh"
    install -m 0644 "${SCRIPT_DIR}/bladerunner-watchdog.service" "${MNT}/etc/systemd/system/bladerunner-watchdog.service"
    install -m 0644 "${SCRIPT_DIR}/bladerunner-vsock-ntp.service" "${MNT}/etc/systemd/system/bladerunner-vsock-ntp.service"
    install -m 0644 "${SCRIPT_DIR}/chrony.conf" "${MNT}/root/bladerunner-chrony.conf"

    chroot "${MNT}" /bin/bash -eu <<EOS
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y incus incus-ui-canonical socat jq openssh-server chrony
systemctl enable incus incus.socket ssh
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
systemctl enable bladerunner-vsock-ntp.service
printf '%s\n' '${INITRAMFS_MODULES}' >> /etc/initramfs-tools/modules
printf '%s' '${BUILD_DATE}' > /etc/bladerunner-image-version
update-initramfs -u
EOS

    if [[ -n "${BR_AGENT_BINARY}" ]]; then
        install -m 0755 "${BR_AGENT_BINARY}" "${MNT}/usr/local/bin/br-agent"
        install -m 0644 "${SCRIPT_DIR}/br-agent.service" "${MNT}/etc/systemd/system/br-agent.service"
        chroot "${MNT}" systemctl enable br-agent.service
    fi
fi

# ----- sparsify and compress ----------------------------------------------

OUT_TMP="${WORK_DIR}/out.qcow2"
if command -v virt-sparsify >/dev/null 2>&1; then
    log "sparsifying with virt-sparsify"
    virt-sparsify --compress "${BASE_IMAGE}" "${OUT_TMP}"
else
    log "virt-sparsify not available; falling back to qemu-img convert"
    qemu-img convert -O qcow2 -c "${BASE_IMAGE}" "${OUT_TMP}"
fi

mv -- "${OUT_TMP}" "${OUTPUT}"

# ----- emit checksum ------------------------------------------------------

DIGEST="$(sha256sum -- "${OUTPUT}" | awk '{print $1}')"
log "built ${OUTPUT} (${DIGEST})"
printf '%s  %s\n' "${DIGEST}" "$(basename -- "${OUTPUT}")" > "${OUTPUT}.sha256"
printf '%s\n' "${DIGEST}"
