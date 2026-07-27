#!/usr/bin/env bash
# Live end-to-end smoke test for GOAL 1 of the cartridge runtime: the holder
# process owns the VM and OUTLIVES whatever spawned it.
#
#   spawn a holder (detached, via a throwaway spawner process)
#     -> assert it registered itself and answers on its control socket
#     -> SIGKILL the SPAWNER
#     -> assert the VM is still running, still reachable, still registered
#     -> drain it cleanly
#     -> assert the registry entry is retracted
#
# The SIGKILL is the point of the whole script. A holder that dies with its
# parent is the failure mode this refactor exists to remove, and it is invisible
# to unit tests: nothing about `br vmd` looks different until the parent goes
# away.
#
# Slow (boots a real VM, and downloads a guest image the first time): budget
# ~5-15 minutes. Needs a codesigned binary (the script runs `make sign`),
# network on first run, and Apple Silicon macOS.
#
# Safe to run repeatedly: the slot directory is reused (so the base image is
# downloaded once), and a holder left behind by an earlier run is drained before
# a new one starts.
#
# Env overrides:
#   SMOKE_READY_TIMEOUT   seconds to wait for guest SSH readiness (default 900).
#   SMOKE_SOCKET_TIMEOUT  seconds to wait for the control socket (default 180).
#   SMOKE_DRAIN_TIMEOUT   seconds to let the guest power itself off (default 120).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
BIN="$PROJECT_ROOT/bin/br"

READY_TIMEOUT="${SMOKE_READY_TIMEOUT:-900}"
SOCKET_TIMEOUT="${SMOKE_SOCKET_TIMEOUT:-180}"
DRAIN_TIMEOUT="${SMOKE_DRAIN_TIMEOUT:-120}"
NAME="smoke-holder"
WORK="$(mktemp -d)"

STATE_ROOT="${BLADERUNNER_STATE_DIR:-$HOME/.local/state/bladerunner}"
SLOT="$STATE_ROOT/disks/$NAME"        # the holder's --state-dir; kept between runs
REGISTRY="$STATE_ROOT/instances/$NAME.json"
SOCKET="$SLOT/control.sock"
HOLDER_LOG="$WORK/holder.log"
PIDFILE="$WORK/holder.pid"
SPAWNER="$WORK/spawner.sh"

SPAWNER_PID=""
HOLDER_PID=""
PASS=0

note() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
ok()   { printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; }
fail() { printf '\033[1;31m  ✗ %s\033[0m\n' "$*" >&2; exit 1; }

alive() { [[ -n "${1:-}" ]] && kill -0 "$1" 2>/dev/null; }

# listed reports whether `br instances` shows our instance. That listing is
# filtered by a live control-socket dial, so a hit means "registered AND
# answering", which is exactly the property under test.
listed() { "$BIN" instances --json 2>/dev/null | grep -qF "\"name\": \"$NAME\""; }

cleanup() {
  local rc=$?
  set +e
  if alive "$SPAWNER_PID"; then
    kill -9 "$SPAWNER_PID" 2>/dev/null
    wait "$SPAWNER_PID" 2>/dev/null
  fi
  if alive "$HOLDER_PID"; then
    note "cleanup: draining the holder (pid $HOLDER_PID)"
    "$BIN" stop --instance "$NAME" --force >/dev/null 2>&1
    for _ in $(seq 1 15); do alive "$HOLDER_PID" || break; sleep 2; done
    alive "$HOLDER_PID" && kill -9 "$HOLDER_PID" 2>/dev/null
  fi
  if [[ "$PASS" -eq 1 && "$rc" -eq 0 ]]; then
    rm -rf "$WORK"
    printf '\n\033[1;32m==> SMOKE PASSED\033[0m\n'
  else
    printf '\n\033[1;31m==> SMOKE FAILED (see above)\033[0m\n' >&2
    printf '    work dir kept for diagnosis: %s\n' "$WORK" >&2
    [[ -f "$HOLDER_LOG" ]] && printf '    holder log tail:\n' >&2 && tail -n 30 "$HOLDER_LOG" >&2
  fi
}
trap cleanup EXIT

note "Preflight: Apple Silicon macOS with a codesigned binary"
[[ "$(uname -s)" == "Darwin" ]] || fail "this smoke test needs macOS (got $(uname -s)) — the holder runs a Virtualization.framework VM"
[[ "$(uname -m)" == "arm64" ]] || fail "this smoke test needs Apple Silicon (got $(uname -m))"
make -C "$PROJECT_ROOT" sign >/dev/null
[[ -x "$BIN" ]] || fail "binary not built at $BIN"
codesign -d --entitlements - "$BIN" 2>&1 | grep -qa "virtualization" \
  || fail "$BIN is not signed with the virtualization entitlement — 'make sign' it"
ok "signed binary ready ($BIN)"

note "Preflight: no holder left over from an earlier run"
# Idempotent: an instance still registered from a previous run is drained rather
# than tripping the "already running" guard on the control socket.
if listed; then
  printf '    a previous %s is still running; draining it\n' "$NAME"
  "$BIN" stop --instance "$NAME" >/dev/null 2>&1 || true
  for _ in $(seq 1 30); do listed || break; sleep 2; done
  listed && fail "could not drain the leftover $NAME instance; stop it by hand"
fi
# A registry entry with no live holder is stale; `br instances` prunes it.
"$BIN" instances >/dev/null 2>&1 || true
[[ -e "$REGISTRY" ]] && fail "stale registry entry survived pruning: $REGISTRY"
mkdir -p "$SLOT"
ok "clean slate (slot: $SLOT)"

note "Spawning a holder from a throwaway spawner process"
# The spawner starts `br vmd` in the background and then blocks forever, so the
# test has a parent process it can kill while the holder is still up. Killing it
# is the assertion; nothing else in this script cares about the spawner.
cat >"$SPAWNER" <<'SPAWNER_EOF'
#!/usr/bin/env bash
set -euo pipefail
bin="$1"; state="$2"; log="$3"; pidfile="$4"; drain="$5"
"$bin" vmd --state-dir "$state" --drain-timeout "${drain}s" </dev/null >>"$log" 2>&1 &
echo $! >"$pidfile"
# Stay alive so the smoke test kills a LIVE parent, not an already-dead one.
while :; do sleep 1; done
SPAWNER_EOF
chmod +x "$SPAWNER"
"$SPAWNER" "$BIN" "$SLOT" "$HOLDER_LOG" "$PIDFILE" "$DRAIN_TIMEOUT" &
SPAWNER_PID=$!
for _ in $(seq 1 20); do [[ -s "$PIDFILE" ]] && break; sleep 1; done
[[ -s "$PIDFILE" ]] || fail "spawner never recorded a holder pid"
HOLDER_PID="$(cat "$PIDFILE")"
alive "$HOLDER_PID" || fail "holder exited immediately — see $HOLDER_LOG"
ok "spawner pid $SPAWNER_PID, holder pid $HOLDER_PID (log: $HOLDER_LOG)"

note "Waiting for the holder to register itself and answer (≤ ${SOCKET_TIMEOUT}s)"
deadline=$(( SECONDS + SOCKET_TIMEOUT ))
registered=0
while (( SECONDS < deadline )); do
  alive "$HOLDER_PID" || fail "holder exited while starting — see $HOLDER_LOG"
  if [[ -e "$REGISTRY" ]] && listed; then registered=1; break; fi
  sleep 2
done
[[ "$registered" -eq 1 ]] || fail "holder did not register within ${SOCKET_TIMEOUT}s — see $HOLDER_LOG"
[[ -S "$SOCKET" ]] || fail "no control socket at $SOCKET"
ok "registry entry published at $REGISTRY"
ok "control socket bound and answering"
grep -F "\"pid\": $HOLDER_PID" "$REGISTRY" >/dev/null \
  || fail "registry entry does not record the holder pid $HOLDER_PID"
ok "registry entry names the holder process"
printf '    ports this instance took:\n'
"$BIN" instances 2>/dev/null | sed 's/^/      /'

note "Killing the SPAWNER — the holder must not care"
kill -9 "$SPAWNER_PID"
wait "$SPAWNER_PID" 2>/dev/null || true
for _ in $(seq 1 10); do alive "$SPAWNER_PID" || break; sleep 1; done
alive "$SPAWNER_PID" && fail "spawner survived SIGKILL (impossible; check the pid plumbing)"
SPAWNER_PID=""
ok "spawner is gone"

# The two independent things the goal actually claims: the process is alive, and
# it is still serving. A holder that is alive but wedged fails the second.
alive "$HOLDER_PID" || fail "HOLDER DIED WITH ITS SPAWNER — this is goal 1 regressing. See $HOLDER_LOG"
ok "holder pid $HOLDER_PID is still alive"
listed || fail "holder no longer answers on its control socket after the spawner died"
ok "holder still answers on its control socket"

note "Waiting for the orphaned VM to finish booting to SSH readiness (≤ ${READY_TIMEOUT}s)"
# Boot completes with no parent process in existence, which is the strongest
# available evidence that the VM is genuinely owned by the holder alone.
deadline=$(( SECONDS + READY_TIMEOUT ))
ready=0
while (( SECONDS < deadline )); do
  alive "$HOLDER_PID" || fail "holder exited while booting — see $HOLDER_LOG"
  if "$BIN" shell --instance "$NAME" -- true >/dev/null 2>&1; then ready=1; break; fi
  sleep 10
done
[[ "$ready" -eq 1 ]] || fail "guest did not become SSH-ready within ${READY_TIMEOUT}s — see $HOLDER_LOG"
ok "guest booted and is reachable — with no parent process anywhere"

note "Draining the holder cleanly (ACPI, wait for stopped, no power cut)"
"$BIN" stop --instance "$NAME" --timeout "$DRAIN_TIMEOUT" || fail "'br stop --instance $NAME' failed"
for _ in $(seq 1 30); do alive "$HOLDER_PID" || break; sleep 2; done
alive "$HOLDER_PID" && fail "holder still running after a clean stop"
HOLDER_PID=""
ok "holder exited"

note "Asserting the instance retracted itself"
# Checked BEFORE `br instances` runs, so this proves the holder removed its own
# entry on the way out rather than a reader pruning a corpse.
[[ -e "$REGISTRY" ]] && fail "registry entry survived the drain: $REGISTRY"
ok "registry entry retracted"
[[ -S "$SOCKET" ]] && fail "control socket survived the drain: $SOCKET"
ok "control socket removed"
listed && fail "'br instances' still lists $NAME after the drain"
ok "'br instances' no longer lists it"

PASS=1
