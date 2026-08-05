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
# Slow (downloads a guest image and boots a VM): budget ~15-25 minutes. The pack
# step alone takes ~7.5 minutes on a cold image cache. Do NOT run this under an
# outer timeout shorter than that: killing the script makes it look like a boot
# failure when the guest was simply still coming up. Needs a
# codesigned binary (the script runs `make sign`), network, and macOS hdiutil.
#
# Env overrides:
#   SMOKE_DISK   builtin/user disk to pack (default: debian-trixie-gui — the
#                incus builtin needs the not-yet-published hosted guest image).
#   SMOKE_READY_TIMEOUT  seconds to wait for guest SSH readiness (default 600).
#   SMOKE_BOOT_ARGS      extra flags for `br boot`. --private-mount is the one
#                        worth setting here: it attaches -nobrowse at
#                        $PRIVATE_MNT, which resolve_mnt already handles, so the
#                        script passes under either policy. Leave it unset to
#                        exercise the browsable default that Finder-eject needs.
#                        Do NOT set --persist: this script packs a throwaway
#                        cartridge and asserts a clean cold-boot state, and
#                        --persist would rewrite the .dmg on the way out.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
BIN="$PROJECT_ROOT/bin/br"

# Cleanup traps and interruptible long commands live in one place, shared with
# smoke-holder.sh and exercised by scripts/test-cleanup-traps.sh.
# shellcheck source=scripts/lib/cleanup-traps.sh
. "$SCRIPT_DIR/lib/cleanup-traps.sh"

DISK="${SMOKE_DISK:-debian-trixie-gui}"
READY_TIMEOUT="${SMOKE_READY_TIMEOUT:-600}"
SHARE_TIMEOUT="${SMOKE_SHARE_TIMEOUT:-300}"  # the runcmd configures the share after SSH is up
NAME="smoke-cartridge"
VOLNAME="bladerunner-$NAME"                  # what `hdiutil create -volname` bakes in
WORK="$(mktemp -d)"
CART="$WORK/${NAME}.sparseimage"
INSPECT="$WORK/inspect"                      # where the layout assertion attaches privately
# `read` returns 1 at EOF, which `set -e` would treat as fatal for an
# unset SMOKE_BOOT_ARGS; the || true keeps an empty array empty.
read -r -a BOOT_ARGS <<< "${SMOKE_BOOT_ARGS:-}" || true

STATE_DIR="${BLADERUNNER_STATE_DIR:-$HOME/.local/state/bladerunner}"
PRIVATE_MNT="$STATE_DIR/mnt/$NAME"     # where --private-mount (and `disk pack`) attach
BROWSABLE_MNT="/Volumes/$VOLNAME"      # where the browsable default lands
REGISTRY="$STATE_DIR/instances/$NAME.json"  # the holder records its pid here
MNT=""                                 # resolved after boot; see resolve_mnt
SHARE=""                               # host side of the RW VirtioFS share
BROWSE_DEV=""                          # /dev node of the browsable-policy attach
BROWSE_MNT=""                          # its mountpoint

HOLDER_PID=""                          # the `br vmd` that owns the VM after boot returns
PASS=0
RECOVERY=""                            # steps cleanup could not take itself, printed at the end

# Cleanup budgets. The eject timeout is what the guest gets for a graceful ACPI
# shutdown; the poll tries bound the wait for the holder to leave afterwards.
EJECT_TIMEOUT=90        # seconds handed to `br eject --timeout`
HOLDER_POLL_TRIES=30    # x HOLDER_POLL seconds waiting for the holder to exit
HOLDER_POLL=2
DETACH_TRIES=5          # x DETACH_WAIT seconds of graceful detach attempts
DETACH_WAIT=2

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
  if [[ -n "$mp" && -d "$mp" ]]; then printf '%s\n' "$mp"; return 0; fi
  # --private-mount (and any future policy that dictates a location) puts the
  # volume outside /Volumes, where hdiutil info still reports it by path.
  for mp in "$PRIVATE_MNT" "$BROWSABLE_MNT"; do
    [[ -d "$mp/share" ]] && { printf '%s\n' "$mp"; return 0; }
  done
  # "Not mounted yet" is the normal case while the guest is still attaching, so
  # it must be success-with-no-output, NOT a non-zero return. Without this the
  # last command evaluated is the failing [[ -d ]] above, the function returns
  # 1, and `MNT="$(resolve_mnt)"` under `set -e` kills the whole script mid-wait
  # — silently, because no `fail` ever runs. The caller already treats an empty
  # result as "keep waiting".
  return 0
}

# --- cleanup ---------------------------------------------------------------
#
# cleanup runs on EXIT, INT, TERM and HUP (scripts/lib/cleanup-traps.sh). A bare
# `trap cleanup EXIT` did not: bash skips an EXIT trap when a signal it has no
# handler for kills the shell, so a CI cancellation or a Ctrl-C left a VM
# running, a cartridge attached and a volume mounted on a shared machine (#227).
#
# Nothing below forces anything. The old fallback ran a FORCED hdiutil detach
# across five candidate mountpoints after a best-effort eject and a SIGKILL,
# with no evidence that the VMM had released the disk. Forcing a detach out from
# under a live writer is the damage AGENTS.md section 8 ("Data safety") exists
# to prevent, so a volume that will not release is PRESERVED and reported.

# recover records a step a human has to take on this host.
recover() { RECOVERY="$RECOVERY    - $1"$'\n'; }

holder_alive() { [[ -n "$HOLDER_PID" ]] && kill -0 "$HOLDER_PID" 2>/dev/null; }

# registry_pid prints the holder pid recorded in a registry entry.
registry_pid() {
  sed -n 's/.*"pid"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$1" | head -1
}

# discover_holder fills HOLDER_PID from the registry when the script has not
# read it yet. A signal during `br boot` leaves a holder behind that this script
# never learned the pid of, and an unknown holder is precisely the one that gets
# orphaned.
discover_holder() {
  if [[ -n "$HOLDER_PID" ]]; then return 0; fi
  if [[ ! -e "$REGISTRY" ]]; then return 0; fi
  HOLDER_PID="$(registry_pid "$REGISTRY" 2>/dev/null)"
  if [[ -n "$HOLDER_PID" ]]; then
    note "cleanup: found holder pid $HOLDER_PID recorded at $REGISTRY"
  fi
  return 0
}

# wait_for_holder_exit waits for the holder to leave. A holder that exits has
# powered the guest off and detached the cartridge itself, so its exit is also
# the evidence that the image was RELEASED — which is what makes any later
# detach safe rather than a guess.
wait_for_holder_exit() {
  local _
  for _ in $(seq 1 "$HOLDER_POLL_TRIES"); do
    if ! holder_alive; then HOLDER_PID=""; return 0; fi
    sleep "$HOLDER_POLL"
  done
  if ! holder_alive; then HOLDER_PID=""; return 0; fi
  return 1
}

# stop_guest asks the guest to power itself off and waits for the holder to go.
# It escalates from a graceful ACPI eject to --force (still a real stop, driven
# through the VMM) but never to SIGKILL: killing the holder is a power cut that
# also leaves the image attached with no owner — the state that then tempts a
# forced detach.
stop_guest() {
  discover_holder
  if ! holder_alive; then return 0; fi
  note "cleanup: ejecting '$NAME' (graceful ACPI shutdown; holder pid $HOLDER_PID)"
  "$BIN" eject "$NAME" --timeout "${EJECT_TIMEOUT}s" >/dev/null 2>&1
  if wait_for_holder_exit; then ok "holder exited; the cartridge was released"; return 0; fi

  note "cleanup: the guest did not stop in time; forcing the stop through the VMM"
  "$BIN" eject "$NAME" --force >/dev/null 2>&1
  if wait_for_holder_exit; then ok "holder exited; the cartridge was released"; return 0; fi

  recover "holder pid $HOLDER_PID STILL owns the VM and its disk. Inspect it with:"
  recover "    ps -p $HOLDER_PID -o pid,etime,command"
  recover "  then stop it with: '$BIN' eject '$NAME' --force"
  recover "  The cartridge is deliberately left attached — detaching it out from under a"
  recover "  live VMM is how the image gets corrupted."
  return 1
}

# attached reports whether hdiutil still knows about a path or /dev node.
attached() { [[ -n "$1" ]] && hdiutil info 2>/dev/null | grep -qF "$1"; }

# detach_gently detaches an attachment WITHOUT -force. A detach that still
# refuses after DETACH_TRIES means something is holding the volume; the
# attachment is preserved and reported rather than ripped out.
detach_gently() {
  local target="$1" what="$2" _
  if ! attached "$target"; then return 0; fi
  for _ in $(seq 1 "$DETACH_TRIES"); do
    hdiutil detach "$target" >/dev/null 2>&1
    if ! attached "$target"; then return 0; fi
    sleep "$DETACH_WAIT"
  done
  recover "$what is still attached at '$target'. Once nothing is using it: hdiutil detach '$target'"
  return 1
}

cleanup() {
  local rc="$1" mp
  set +e
  # An interrupted `wait` leaves the long command it was watching running and
  # unreaped; this ends it and reaps it exactly once.
  reap_interruptible_child

  stop_guest || rc=1

  # Attachments this script made itself. No VM ever wrote to them, and both are
  # already detached on the happy path.
  detach_gently "$INSPECT" "the layout inspection mount" || rc=1
  detach_gently "$BROWSE_DEV" "the browsable-policy mount" || rc=1
  detach_gently "$BROWSE_MNT" "the browsable-policy mount" || rc=1

  # The BOOTED cartridge belongs to the holder, which detaches it on the way
  # out. Touch it only once the holder is positively gone: ownership is
  # established before the detach rather than assumed.
  if holder_alive; then
    recover "the booted cartridge is left mounted on purpose while pid $HOLDER_PID still owns it"
    rc=1
  else
    for mp in "$MNT" "$BROWSABLE_MNT" "$PRIVATE_MNT"; do
      detach_gently "$mp" "the booted cartridge" || rc=1
    done
  fi

  if [[ "$PASS" -eq 1 && "$rc" -eq 0 ]]; then
    rm -rf "$WORK"
    printf '\n\033[1;32m==> SMOKE PASSED\033[0m\n'
    return 0
  fi
  # A run that never reached PASS=1 failed, whatever status brought us here.
  [[ "$rc" -eq 0 ]] && rc=1
  # Preserve the work dir (incl. boot.log) for diagnosis on failure.
  printf '\n\033[1;31m==> SMOKE FAILED (see above)\033[0m\n' >&2
  printf '    work dir kept for diagnosis: %s\n' "$WORK" >&2
  [[ -f "$WORK/boot.log" ]] && printf '    boot log tail:\n' >&2 && tail -n 25 "$WORK/boot.log" >&2
  if [[ -n "$RECOVERY" ]]; then
    printf '\n\033[1;31m==> MANUAL RECOVERY NEEDED on this host:\033[0m\n' >&2
    printf '%s' "$RECOVERY" >&2
  fi
  return "$rc"
}
install_cleanup_traps

note "Building + codesigning bladerunner"
make -C "$PROJECT_ROOT" sign >/dev/null
[[ -x "$BIN" ]] || fail "binary not built at $BIN"
ok "signed binary ready"

# Ports are a PREFERENCE, not a fixed allocation: the default instance prefers
# 6022/18443 and any instance that finds them taken falls back to ephemeral ones
# (internal/portalloc.Reserve). So a second VM no longer makes this boot FAIL —
# it makes it succeed on ports this script did not expect, which is worse to
# diagnose. The check below therefore stays: it keeps the smoke run on the
# well-known ports it asserts against.
note "Preflight: required local ports must be free (the default instance only PREFERS these; another VM holding them silently pushes this boot onto ephemeral ports)"
port_in_use() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null && { exec 3>&-; return 0; } || return 1; }
for p in 6022 18443; do
  if port_in_use "$p"; then
    fail "port $p is in use — another bladerunner VM is running. Stop it ('br stop') first; the cartridge boot needs these ports."
  fi
done
ok "local ports free"

note "Packing a cartridge from '$DISK' (downloads image, bakes root.img, real hdiutil) + --ship"
run_interruptible "$BIN" disk pack "$DISK" --out "$CART" --ship
[[ -f "$CART" ]] || fail "pack did not produce $CART"
DMG="${CART%.sparseimage}.dmg"
[[ -f "$DMG" ]] || fail "--ship did not produce $DMG"
ok "packed $(basename "$CART") + $(basename "$DMG")"

note "Asserting cartridge layout (PRIVATE policy: attach read-only at a dictated mountpoint, check files, detach)"
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
# Forget the device now that it is released. A /dev/diskN number is recycled,
# and cleanup must never detach a node that some other image has since taken.
BROWSE_DEV=""
BROWSE_MNT=""
ok "browsable mount detached"

note "Booting the cartridge headless (the VM runs under a holder, not under br boot)"
# `br boot` now spawns a `br vmd` holder, attaches to it, and RETURNS once the
# guest is up. It blocks for the whole boot, so this is a foreground call.
run_interruptible "$BIN" boot "$CART" --headless --timeout "${READY_TIMEOUT}s" ${BOOT_ARGS[@]+"${BOOT_ARGS[@]}"} >"$WORK/boot.log" 2>&1 \
  || { sed 's/^/      /' "$WORK/boot.log" >&2; fail "br boot failed — see $WORK/boot.log"; }
ok "br boot returned (log: $WORK/boot.log)"

note "GOAL 1: br boot has EXITED and the VM is still running"
# This is the assertion the whole refactor exists for. Before it, `br boot` WAS
# the VM: the process that ran this command owned the VMM, and it had to stay
# alive for the rest of the script. Now it is gone and a holder owns the VM.
[[ -e "$REGISTRY" ]] || fail "no registry entry at $REGISTRY after boot returned"
HOLDER_PID="$(registry_pid "$REGISTRY")"
[[ -n "$HOLDER_PID" ]] || fail "registry entry names no holder pid: $REGISTRY"
kill -0 "$HOLDER_PID" 2>/dev/null || fail "the holder (pid $HOLDER_PID) is not running after br boot returned"
[[ "$HOLDER_PID" != "$$" ]] || fail "the holder pid is this script — the VM is not detached"
ok "holder pid $HOLDER_PID owns the VM; br boot is gone"

MNT="$(resolve_mnt)"
[[ -n "$MNT" ]] || fail "cartridge is not mounted after boot returned"
SHARE="$MNT/share"
ok "cartridge mounted at $MNT"
BLADERUNNER_STATE_DIR="$MNT" "$BIN" shell -- true >/dev/null 2>&1 \
  || fail "guest is not reachable even though br boot reported it ready"
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
  if ! kill -0 "$HOLDER_PID" 2>/dev/null; then fail "the holder exited while waiting for the share — see $WORK/boot.log"; fi
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
run_interruptible "$BIN" eject "$NAME"
# The HOLDER powers the guest off, detaches, and exits. (It is not a child of
# this shell, so it is polled rather than waited on.)
wait_for_holder_exit || fail "the holder is still running after eject"
ok "guest powered off cleanly and the holder exited"

if hdiutil info 2>/dev/null | grep -q "bladerunner-$NAME"; then
  fail "cartridge still attached after eject"
fi
ok "cartridge detached — left in a clean cold-boot state, ready to AirDrop"

PASS=1
