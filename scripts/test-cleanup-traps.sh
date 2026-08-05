#!/usr/bin/env bash
# test-cleanup-traps.sh — prove the smoke-test cleanup traps actually fire.
#
# scripts/lib/cleanup-traps.sh is the one place `scripts/smoke-cartridge.sh` and
# `scripts/smoke-holder.sh` install their traps. Both used to register EXIT
# only, so a CI cancellation or a Ctrl-C skipped cleanup and stranded a VM, an
# attached cartridge and a mounted volume on a shared machine (#227).
#
# This test needs no VM and no hardware. It sources the SAME library the smoke
# scripts source, runs a subject script under it, signals that subject, and
# asserts from the outside that cleanup ran — once, with the right status, and
# that the subject still died of the signal it was sent.
#
# Exit status: 0 when every case passes, 1 otherwise.

set -euo pipefail

# Job control ON. Without it bash sets SIGINT and SIGQUIT to SIG_IGN in every
# background job of a non-interactive shell, and a signal ignored on entry
# cannot be trapped at all — the subject would be untestable for INT for a
# reason that has nothing to do with the code under test. `set -m` gives each
# subject its own process group and its default signal dispositions.
set -m

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB="$SCRIPT_DIR/lib/cleanup-traps.sh"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# READY_TRIES x READY_SLEEP bounds the wait for a subject to arm its traps;
# SETTLE_SLEEP lets a just-started subject reach its `wait`.
READY_TRIES=100
READY_SLEEP=0.1
SETTLE_SLEEP=0.5

fails=0

pass() { printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; }
bad()  { printf '\033[1;31m  ✗ %s\033[0m\n' "$*" >&2; fails=$((fails + 1)); }
note() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }

[[ -f "$LIB" ]] || { printf 'missing library: %s\n' "$LIB" >&2; exit 2; }

# The subject blocks the way the smoke scripts block: on a background child via
# `wait`, not on a foreground command. That is the arrangement run_interruptible
# creates, and it is what lets a trap arrive while a long command is in flight.
SUBJECT="$WORK/subject.sh"
cat >"$SUBJECT" <<'SUBJECT_EOF'
#!/usr/bin/env bash
set -euo pipefail
lib="$1"; marker="$2"; ready="$3"; mode="$4"
# shellcheck source=/dev/null
. "$lib"

cleanup() {
  local status="$1"
  printf 'cleanup status=%s child=%s\n' "$status" "${CLEANUP_CHILD_PID:-none}" >>"$marker"
  reap_interruptible_child
  return 0
}

install_cleanup_traps
: >"$ready"

case "$mode" in
  exit0) exit 0 ;;
  exit3) exit 3 ;;
  block) run_interruptible sleep 300 ;;
esac
SUBJECT_EOF
chmod +x "$SUBJECT"

# run_subject starts the subject in mode $1 and sets SUBJECT_PID once the
# subject reports that its traps are armed. It assigns a global rather than
# printing the pid: a command substitution would fork, and the job would then
# belong to that subshell instead of to this one, so `wait` could not reap it.
SUBJECT_PID=""
run_subject() {
  local mode="$1" marker="$2" ready="$3" i
  rm -f "$marker" "$ready"
  "$SUBJECT" "$LIB" "$marker" "$ready" "$mode" &
  SUBJECT_PID=$!
  for ((i = 0; i < READY_TRIES; i++)); do
    [[ -e "$ready" ]] && break
    sleep "$READY_SLEEP"
  done
}

# cleanup_runs counts the cleanup lines the subject wrote. Exactly one is the
# whole point: the signal handler runs cleanup and the EXIT trap must not run it
# again.
cleanup_runs() { grep -c '^cleanup status=' "$1" 2>/dev/null || printf '0\n'; }

note "EXIT still works (a plain exit must keep running cleanup)"
for spec in "exit0 0" "exit3 3"; do
  read -r mode want <<<"$spec"
  marker="$WORK/$mode.marker"; ready="$WORK/$mode.ready"
  run_subject "$mode" "$marker" "$ready"
  rc=0; wait "$SUBJECT_PID" || rc=$?
  if [[ "$rc" -ne "$want" ]]; then bad "$mode: exit status $rc, wanted $want"; continue; fi
  if [[ "$(cleanup_runs "$marker")" -ne 1 ]]; then
    bad "$mode: cleanup ran $(cleanup_runs "$marker") times, wanted 1"
    continue
  fi
  if ! grep -q "^cleanup status=$want " "$marker"; then
    bad "$mode: cleanup did not see status $want: $(cat "$marker")"
    continue
  fi
  pass "$mode: cleanup ran once with status $want, script exited $rc"
done

note "INT, TERM and HUP must run cleanup and still die of the signal"
# 128+n is the conventional status for death by signal n; bash's `wait` reports
# it, and a handler that swallowed the signal would report something else.
for spec in "INT 2" "TERM 15" "HUP 1"; do
  read -r sig num <<<"$spec"
  want=$((128 + num))
  marker="$WORK/$sig.marker"; ready="$WORK/$sig.ready"
  run_subject block "$marker" "$ready"
  if ! kill -0 "$SUBJECT_PID" 2>/dev/null; then bad "SIG$sig: subject died before it was signalled"; continue; fi
  kill -s "$sig" "$SUBJECT_PID"
  rc=0; wait "$SUBJECT_PID" || rc=$?
  if [[ "$(cleanup_runs "$marker")" -eq 0 ]]; then
    bad "SIG$sig: CLEANUP DID NOT RUN — this is #227 regressing"
    continue
  fi
  if [[ "$(cleanup_runs "$marker")" -ne 1 ]]; then
    bad "SIG$sig: cleanup ran $(cleanup_runs "$marker") times, wanted 1"
    continue
  fi
  if [[ "$rc" -ne "$want" ]]; then
    bad "SIG$sig: exit status $rc, wanted $want (the signal must be re-raised, not swallowed)"
    continue
  fi
  if ! grep -q 'child=[0-9]' "$marker"; then
    bad "SIG$sig: cleanup saw no in-flight child; the trap arrived after the wait, not during it"
    continue
  fi
  pass "SIG$sig: cleanup ran once while a child was in flight, script exited $rc (128+$num)"
done

note "the interrupted child must be reaped, not left running"
marker="$WORK/orphan.marker"; ready="$WORK/orphan.ready"
run_subject block "$marker" "$ready"
sleep "$SETTLE_SLEEP"
# The subject's own child is the `sleep 300` run_interruptible started.
kid="$(pgrep -P "$SUBJECT_PID" 2>/dev/null | head -1 || true)"
kill -s TERM "$SUBJECT_PID"
wait "$SUBJECT_PID" 2>/dev/null || true
if [[ -z "$kid" ]]; then
  bad "could not identify the subject's child; the reaping assertion did not run"
else
  gone=0
  for _ in $(seq 1 20); do kill -0 "$kid" 2>/dev/null || { gone=1; break; }; sleep "$SETTLE_SLEEP"; done
  if [[ "$gone" -eq 1 ]]; then
    pass "the in-flight child (pid $kid) was terminated by cleanup"
  else
    bad "the in-flight child (pid $kid) outlived the signalled script"
  fi
fi

note "the smoke scripts must use the shared traps and must not force anything"
# The library above is only useful if the real scripts actually use it, and the
# other half of #227 was the forced detach. Both are checked in the files
# themselves so a regression is caught here rather than on a Mac mini.
for subject in smoke-cartridge.sh smoke-holder.sh; do
  f="$SCRIPT_DIR/$subject"
  clean=1
  if ! grep -q 'lib/cleanup-traps\.sh' "$f"; then
    bad "$subject does not source the shared trap library"; clean=0
  fi
  if ! grep -q '^install_cleanup_traps$' "$f"; then
    bad "$subject never installs the traps"; clean=0
  fi
  if grep -q '^trap cleanup EXIT$' "$f"; then
    bad "$subject still registers EXIT only — this is #227 regressing"; clean=0
  fi
  if grep -q 'hdiutil detach.*-force' "$f"; then
    bad "$subject still force-detaches a volume"; clean=0
  fi
  # shellcheck disable=SC2016  # intentional: a grep pattern for the literal text "$HOLDER_PID"
  if grep -q 'kill -9 "\$HOLDER_PID"' "$f"; then
    bad "$subject still SIGKILLs the process that owns a live VM disk"; clean=0
  fi
  [[ "$clean" -eq 1 ]] && pass "$subject: shared traps installed, nothing forced"
done

printf '\n'
if [[ "$fails" -eq 0 ]]; then
  printf '\033[1;32m==> cleanup traps: OK\033[0m\n'
else
  printf '\033[1;31m==> cleanup traps: %d FAILED\033[0m\n' "$fails" >&2
fi
exit $(( fails > 0 ? 1 : 0 ))
