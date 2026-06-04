#!/usr/bin/env bash
# Live end-to-end smoke test for the cartridge feature. Exercises the real
# lifecycle against real hdiutil + a real VM:
#
#   pack -> (assert layout) -> boot headless -> RW host<->guest share round-trip
#        -> ACPI eject -> assert the cartridge detached.
#
# Slow (downloads a guest image and boots a VM): budget ~5-10 minutes. Needs a
# codesigned binary (the script runs `make sign`), network, and macOS hdiutil.
#
# Env overrides:
#   SMOKE_DISK   builtin/user disk to pack (default: debian-trixie-gui — the
#                incus builtin needs the not-yet-published hosted guest image).
#   SMOKE_READY_TIMEOUT  seconds to wait for guest SSH readiness (default 600).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
BIN="$PROJECT_ROOT/bin/bladerunner"

DISK="${SMOKE_DISK:-debian-trixie-gui}"
READY_TIMEOUT="${SMOKE_READY_TIMEOUT:-600}"
NAME="smoke-cartridge"
WORK="$(mktemp -d)"
CART="$WORK/${NAME}.sparseimage"

STATE_DIR="${BLADERUNNER_STATE_DIR:-$HOME/.local/state/bladerunner}"
MNT="$STATE_DIR/mnt/$NAME"          # `boot` mounts the cartridge here
SHARE="$MNT/share"                  # host side of the RW VirtioFS share

BOOT_PID=""
PASS=0

note() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
ok()   { printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; }
fail() { printf '\033[1;31m  ✗ %s\033[0m\n' "$*" >&2; exit 1; }

cleanup() {
  local rc=$?
  set +e
  if [[ -n "$BOOT_PID" ]] && kill -0 "$BOOT_PID" 2>/dev/null; then
    note "cleanup: ejecting (force) and stopping the boot process"
    BLADERUNNER_STATE_DIR="$MNT" "$BIN" eject --force >/dev/null 2>&1
    sleep 3
    kill "$BOOT_PID" 2>/dev/null
    wait "$BOOT_PID" 2>/dev/null
  fi
  # Belt-and-suspenders: detach the cartridge if anything left it mounted.
  hdiutil detach "$MNT" -force >/dev/null 2>&1
  rm -rf "$WORK"
  if [[ "$PASS" -eq 1 && "$rc" -eq 0 ]]; then
    printf '\n\033[1;32m==> SMOKE PASSED\033[0m\n'
  else
    printf '\n\033[1;31m==> SMOKE FAILED (see above; boot log: %s)\033[0m\n' "$WORK/boot.log" >&2
  fi
}
trap cleanup EXIT

note "Building + codesigning bladerunner"
make -C "$PROJECT_ROOT" sign >/dev/null
[[ -x "$BIN" ]] || fail "binary not built at $BIN"
ok "signed binary ready"

note "Packing a cartridge from '$DISK' (downloads image, bakes root.img, real hdiutil) + --ship"
"$BIN" disk pack "$DISK" --out "$CART" --ship
[[ -f "$CART" ]] || fail "pack did not produce $CART"
DMG="${CART%.sparseimage}.dmg"
[[ -f "$DMG" ]] || fail "--ship did not produce $DMG"
ok "packed $(basename "$CART") + $(basename "$DMG")"

note "Asserting cartridge layout (attach read-only, check files, detach)"
INSPECT="$WORK/inspect"
hdiutil attach "$CART" -mountpoint "$INSPECT" -nobrowse -owners on -noverify >/dev/null
layout_ok=1
for f in disk.json root.img state share; do
  if [[ -e "$INSPECT/$f" ]]; then ok "layout has $f"; else printf '  ✗ missing %s\n' "$f" >&2; layout_ok=0; fi
done
hdiutil detach "$INSPECT" >/dev/null
[[ "$layout_ok" -eq 1 ]] || fail "cartridge layout incomplete"

note "Booting the cartridge headless in the background"
"$BIN" boot "$CART" --headless --timeout "${READY_TIMEOUT}s" >"$WORK/boot.log" 2>&1 &
BOOT_PID=$!
ok "boot pid $BOOT_PID (log: $WORK/boot.log)"

note "Waiting for the cartridge to attach + the guest to reach SSH readiness (≤ ${READY_TIMEOUT}s)"
deadline=$(( SECONDS + READY_TIMEOUT ))
ready=0
while (( SECONDS < deadline )); do
  if ! kill -0 "$BOOT_PID" 2>/dev/null; then fail "boot process exited early — see $WORK/boot.log"; fi
  if BLADERUNNER_STATE_DIR="$MNT" "$BIN" shell -- true >/dev/null 2>&1; then ready=1; break; fi
  sleep 10
done
[[ "$ready" -eq 1 ]] || fail "guest did not become SSH-ready within ${READY_TIMEOUT}s"
ok "guest is up and reachable"
[[ -d "$SHARE" ]] || fail "share dir not present at $SHARE"

note "RW VirtioFS share round-trip (host <-> guest)"
HOST_MSG="host-to-guest-$$-$RANDOM"
printf '%s\n' "$HOST_MSG" > "$SHARE/from-host.txt"
got=""
for _ in 1 2 3 4 5 6; do  # the guest mount may settle a beat after SSH
  got="$(BLADERUNNER_STATE_DIR="$MNT" "$BIN" shell -- cat /mnt/share/from-host.txt 2>/dev/null | tr -d '\r\n')"
  [[ "$got" == "$HOST_MSG" ]] && break
  sleep 5
done
[[ "$got" == "$HOST_MSG" ]] || fail "guest did not see host file (got: '$got', want: '$HOST_MSG')"
ok "guest read the host-written file over VirtioFS"

GUEST_MSG="guest-to-host-$$-$RANDOM"
BLADERUNNER_STATE_DIR="$MNT" "$BIN" shell -- sh -c "printf '%s\n' '$GUEST_MSG' > /mnt/share/from-guest.txt"
sleep 2
grep -q "$GUEST_MSG" "$SHARE/from-guest.txt" 2>/dev/null || fail "host did not see guest-written file"
ok "host read the guest-written file over VirtioFS (RW both ways)"

note "Ejecting (ACPI graceful shutdown, then detach)"
BLADERUNNER_STATE_DIR="$MNT" "$BIN" eject
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
