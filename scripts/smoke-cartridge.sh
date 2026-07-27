#!/usr/bin/env bash
# Live end-to-end smoke test for the cartridge feature. Exercises the real
# lifecycle against real hdiutil + a real VM:
#
#   pack -> (assert layout, private policy) -> (assert browsable policy)
#        -> boot headless -> RW host<->guest share round-trip
#        -> ACPI eject -> assert the cartridge detached.
#
# Both mount policies are covered. The layout assertion attaches PRIVATELY
# (-mountpoint, -nobrowse) because that is deterministic and is exactly what
# `br disk pack` does. A separate step attaches the shipped DMG BROWSABLY (no
# -mountpoint) and asserts macOS placed it at /Volumes/bladerunner-<name> — the
# mount that Finder can eject, and therefore the one goals 4 and 5 depend on.
#
# The booted mountpoint is DISCOVERED rather than assumed: under the browsable
# default `br boot` lands under /Volumes (with a " 1" collision suffix if a
# volume of that name is already mounted), and with --private-mount it lands at
# <state>/mnt/<name>. resolve_mnt() handles both, so this script keeps passing
# whichever default is in force.
#
# Slow (downloads a guest image and boots a VM): budget ~5-10 minutes. Needs a
# codesigned binary (the script runs `make sign`), network, and macOS hdiutil.
#
# Env overrides:
#   SMOKE_DISK   builtin/user disk to pack (default: debian-trixie-gui — the
#                incus builtin needs the not-yet-published hosted guest image).
#   SMOKE_READY_TIMEOUT  seconds to wait for guest SSH readiness (default 600).
#   SMOKE_BOOT_ARGS      extra flags for `br boot` (e.g. --private-mount once
#                        the CLI exposes it).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
BIN="$PROJECT_ROOT/bin/br"

DISK="${SMOKE_DISK:-debian-trixie-gui}"
READY_TIMEOUT="${SMOKE_READY_TIMEOUT:-600}"
SHARE_TIMEOUT="${SMOKE_SHARE_TIMEOUT:-300}"  # the runcmd configures the share after SSH is up
NAME="smoke-cartridge"
VOLNAME="bladerunner-$NAME"                  # what `hdiutil create -volname` bakes in
WORK="$(mktemp -d)"
CART="$WORK/${NAME}.sparseimage"
# `read` returns 1 at EOF, which `set -e` would treat as fatal for an
# unset SMOKE_BOOT_ARGS; the || true keeps an empty array empty.
read -r -a BOOT_ARGS <<< "${SMOKE_BOOT_ARGS:-}" || true

STATE_DIR="${BLADERUNNER_STATE_DIR:-$HOME/.local/state/bladerunner}"
PRIVATE_MNT="$STATE_DIR/mnt/$NAME"     # where --private-mount (and `disk pack`) attach
BROWSABLE_MNT="/Volumes/$VOLNAME"      # where the browsable default lands
MNT=""                                 # resolved after boot; see resolve_mnt
SHARE=""                               # host side of the RW VirtioFS share

BOOT_PID=""
PASS=0

note() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
ok()   { printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; }
fail() { printf '\033[1;31m  ✗ %s\033[0m\n' "$*" >&2; exit 1; }

# mount_line_path extracts the mountpoint from an `hdiutil attach`/`hdiutil info`
# row: tab-separated columns whose last field is the mount path (which may itself
# contain spaces, e.g. the "bladerunner-demo 1" collision suffix).
mount_line_path() { sed 's/.*\/Volumes\//\/Volumes\//'; }

# resolve_mnt prints the directory the booted cartridge is actually mounted at,
# or nothing if it is not mounted yet. The mountpoint is a RESULT of the mount
# policy, never an input: the browsable default lets macOS choose (and suffix on
# collision), so ask `hdiutil info` rather than guessing.
resolve_mnt() {
  local mp
  mp="$(hdiutil info 2>/dev/null | grep -m1 "/Volumes/$VOLNAME" | mount_line_path)"
  if [[ -n "$mp" && -d "$mp" ]]; then printf '%s\n' "$mp"; return; fi
  # --private-mount (and any future policy that dictates a location) puts the
  # volume outside /Volumes, where hdiutil info still reports it by path.
  for mp in "$PRIVATE_MNT" "$BROWSABLE_MNT"; do
    [[ -d "$mp/share" ]] && { printf '%s\n' "$mp"; return; }
  done
}

cleanup() {
  local rc=$?
  set +e
  if [[ -n "$BOOT_PID" ]] && kill -0 "$BOOT_PID" 2>/dev/null; then
    note "cleanup: ejecting (force) and stopping the boot process"
    "$BIN" eject "$NAME" --force >/dev/null 2>&1
    sleep 3
    kill "$BOOT_PID" 2>/dev/null
    wait "$BOOT_PID" 2>/dev/null
  fi
  # Belt-and-suspenders: detach the cartridge if anything left it mounted, at
  # whichever location the policy in force put it.
  for mp in "$MNT" "$BROWSABLE_MNT" "$PRIVATE_MNT" "$WORK/inspect" "$WORK/browsable"; do
    [[ -n "$mp" ]] && hdiutil detach "$mp" -force >/dev/null 2>&1
  done
  if [[ "$PASS" -eq 1 && "$rc" -eq 0 ]]; then
    rm -rf "$WORK"
    printf '\n\033[1;32m==> SMOKE PASSED\033[0m\n'
  else
    # Preserve the work dir (incl. boot.log) for diagnosis on failure.
    printf '\n\033[1;31m==> SMOKE FAILED (see above)\033[0m\n' >&2
    printf '    work dir kept for diagnosis: %s\n' "$WORK" >&2
    [[ -f "$WORK/boot.log" ]] && printf '    boot log tail:\n' >&2 && tail -n 25 "$WORK/boot.log" >&2
  fi
}
trap cleanup EXIT

note "Building + codesigning bladerunner"
make -C "$PROJECT_ROOT" sign >/dev/null
[[ -x "$BIN" ]] || fail "binary not built at $BIN"
ok "signed binary ready"

note "Preflight: required local ports must be free (bladerunner uses fixed ports — one VM at a time)"
port_in_use() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null && { exec 3>&-; return 0; } || return 1; }
for p in 6022 18443; do
  if port_in_use "$p"; then
    fail "port $p is in use — another bladerunner VM is running. Stop it ('br stop') first; the cartridge boot needs these ports."
  fi
done
ok "local ports free"

note "Packing a cartridge from '$DISK' (downloads image, bakes root.img, real hdiutil) + --ship"
"$BIN" disk pack "$DISK" --out "$CART" --ship
[[ -f "$CART" ]] || fail "pack did not produce $CART"
DMG="${CART%.sparseimage}.dmg"
[[ -f "$DMG" ]] || fail "--ship did not produce $DMG"
ok "packed $(basename "$CART") + $(basename "$DMG")"

note "Asserting cartridge layout (PRIVATE policy: attach read-only at a dictated mountpoint, check files, detach)"
INSPECT="$WORK/inspect"
hdiutil attach "$CART" -mountpoint "$INSPECT" -nobrowse -owners on -noverify >/dev/null
layout_ok=1
for f in disk.json root.img state share; do
  if [[ -e "$INSPECT/$f" ]]; then ok "layout has $f"; else printf '  ✗ missing %s\n' "$f" >&2; layout_ok=0; fi
done
hdiutil detach "$INSPECT" >/dev/null
[[ "$layout_ok" -eq 1 ]] || fail "cartridge layout incomplete"

note "Asserting the BROWSABLE policy on the shipped DMG (no -mountpoint: macOS chooses, Finder can eject)"
# This is the mount that goals 4 and 5 hang off. Two things must hold: the
# volume name carries the bladerunner- prefix (or mount detection never even
# looks at it), and macOS places it under /Volumes where a human can eject it.
BROWSE_LINE="$(hdiutil attach "$DMG" -owners on -noverify | grep -m1 '/Volumes/' || true)"
[[ -n "$BROWSE_LINE" ]] || fail "browsable attach mounted nothing under /Volumes"
BROWSE_DEV="$(printf '%s' "$BROWSE_LINE" | cut -f1 | tr -d '[:space:]')"
BROWSE_MNT="$(printf '%s' "$BROWSE_LINE" | mount_line_path)"
[[ -d "$BROWSE_MNT" ]] || fail "browsable attach produced no usable mountpoint (dev '$BROWSE_DEV', line '$BROWSE_LINE')"
case "$BROWSE_MNT" in
  /Volumes/"$VOLNAME"*) ok "browsable mount landed at $BROWSE_MNT (Finder-visible, ejectable)" ;;
  *) fail "browsable mount landed at $BROWSE_MNT, expected /Volumes/$VOLNAME*" ;;
esac
[[ -e "$BROWSE_MNT/disk.json" ]] || fail "browsable mount is missing disk.json — mount detection would ignore it"
ok "volume name matches the bladerunner- prefix mount detection filters on"
hdiutil detach "$BROWSE_DEV" >/dev/null || fail "could not detach the browsable mount"
ok "browsable mount detached"

note "Booting the cartridge headless in the background"
"$BIN" boot "$CART" --headless --timeout "${READY_TIMEOUT}s" ${BOOT_ARGS[@]+"${BOOT_ARGS[@]}"} >"$WORK/boot.log" 2>&1 &
BOOT_PID=$!
ok "boot pid $BOOT_PID (log: $WORK/boot.log)"

note "Waiting for the cartridge to attach + the guest to reach SSH readiness (≤ ${READY_TIMEOUT}s)"
deadline=$(( SECONDS + READY_TIMEOUT ))
ready=0
while (( SECONDS < deadline )); do
  if ! kill -0 "$BOOT_PID" 2>/dev/null; then fail "boot process exited early — see $WORK/boot.log"; fi
  if [[ -z "$MNT" ]]; then
    MNT="$(resolve_mnt)"
    [[ -n "$MNT" ]] && { SHARE="$MNT/share"; ok "cartridge mounted at $MNT"; }
  fi
  if [[ -n "$MNT" ]] && BLADERUNNER_STATE_DIR="$MNT" "$BIN" shell -- true >/dev/null 2>&1; then ready=1; break; fi
  sleep 10
done
[[ "$ready" -eq 1 ]] || fail "guest did not become SSH-ready within ${READY_TIMEOUT}s (mountpoint: ${MNT:-not found})"
ok "guest is up and reachable"
[[ -d "$SHARE" ]] || fail "share dir not present at $SHARE"

note "RW VirtioFS share round-trip (host <-> guest)"
printf '    host share dir: %s\n' "$SHARE"
ls -ld "$SHARE" 2>&1 || true

# SSH comes up early (break-glass), but the cloud-init runcmd configures the
# virtiofs share a bit later in the same boot. Poll the guest until the mount
# actually appears (up to SHARE_TIMEOUT) before asserting the round-trip.
HOST_MSG="host-to-guest-$$-$RANDOM"
if ! printf '%s\n' "$HOST_MSG" > "$SHARE/from-host.txt" 2>/tmp/share-write.err; then
  fail "host could not write to the share dir $SHARE: $(cat /tmp/share-write.err 2>/dev/null)"
fi
ok "host wrote a file into the cartridge share/"
share_deadline=$(( SECONDS + SHARE_TIMEOUT ))
got=""
while (( SECONDS < share_deadline )); do
  if ! kill -0 "$BOOT_PID" 2>/dev/null; then fail "boot process exited while waiting for the share — see $WORK/boot.log"; fi
  got="$(BLADERUNNER_STATE_DIR="$MNT" "$BIN" shell -- cat /mnt/share/from-host.txt 2>/dev/null | tr -d '\r\n' || true)"
  [[ "$got" == "$HOST_MSG" ]] && break
  printf '    waiting for guest virtiofs mount… (%ds left)\n' "$(( share_deadline - SECONDS ))"
  sleep 10
done
if [[ "$got" != "$HOST_MSG" ]]; then
  note "guest virtiofs diagnostics (share never appeared)"
  # Pipe one script to the guest's sh via stdin (avoids per-arg SSH quoting and
  # the stdin-consumption trap of running ssh inside a read loop).
  # shellcheck disable=SC2016  # intentional: this script body runs in the guest, not here
  printf '%s\n' '
set +e
echo "## uname"; uname -a
echo "## virtio modules"; lsmod | grep -i virtio || echo "(none)"
echo "## virtio devices"; ls -l /sys/bus/virtio/devices/ 2>&1
echo "## virtiofs tags"; for f in /sys/bus/virtio/devices/*/; do [ -e "$f/features" ] && echo "$f"; done; cat /sys/fs/*/tag 2>/dev/null
echo "## mount unit file"; cat /etc/systemd/system/mnt-share.mount 2>&1 || echo "(MISSING)"
echo "## mount unit status"; systemctl status mnt-share.mount --no-pager 2>&1 | head -30
echo "## fstab"; cat /etc/fstab
echo "## manual mount attempt"; mount -t virtiofs bladerunner-share /mnt/share 2>&1; echo "rc=$?"; mount | grep -i virtiofs || echo "(still no virtiofs mount)"
echo "## cloud-init"; cloud-init status --long 2>&1 || echo "(na)"
' | BLADERUNNER_STATE_DIR="$MNT" "$BIN" shell -- sh 2>&1 | sed 's/^/      /' || true
  fail "guest did not see host file — virtiofs share not mounted (see diagnostics above)"
fi
ok "guest read the host-written file over VirtioFS"

# guest -> host (pipe the content to the guest's tee via stdin — avoids the
# SSH arg-quoting that mangles `sh -c "printf ..."`).
GUEST_MSG="guest-to-host-$$-$RANDOM"
printf '%s\n' "$GUEST_MSG" | BLADERUNNER_STATE_DIR="$MNT" "$BIN" shell -- tee /mnt/share/from-guest.txt >/dev/null \
  || fail "guest could not write to /mnt/share"
got2=""
for _ in 1 2 3 4; do
  got2="$(cat "$SHARE/from-guest.txt" 2>/dev/null | tr -d '\r\n' || true)"
  [[ "$got2" == "$GUEST_MSG" ]] && break
  sleep 3
done
[[ "$got2" == "$GUEST_MSG" ]] || fail "host did not see guest-written file (got: '$got2')"
ok "host read the guest-written file over VirtioFS (RW both ways)"

note "Ejecting (ACPI graceful shutdown, then detach)"
# Eject by cartridge name (no BLADERUNNER_STATE_DIR override): this exercises the
# real cartridge-scan path and labels the slot by name, rather than treating the
# overridden state dir as the flat "default" slot.
"$BIN" eject "$NAME"
# The foreground boot process powers off, detaches, and exits.
for _ in $(seq 1 30); do kill -0 "$BOOT_PID" 2>/dev/null || break; sleep 2; done
if kill -0 "$BOOT_PID" 2>/dev/null; then fail "boot process still running after eject"; fi
wait "$BOOT_PID" 2>/dev/null
BOOT_PID=""
ok "guest powered off cleanly and the boot process exited"

if hdiutil info 2>/dev/null | grep -q "bladerunner-$NAME"; then
  fail "cartridge still attached after eject"
fi
ok "cartridge detached — left in a clean cold-boot state, ready to AirDrop"

PASS=1
