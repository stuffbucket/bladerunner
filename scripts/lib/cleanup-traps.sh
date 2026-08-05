# cleanup-traps.sh — the one place the real-hardware smoke scripts install their
# cleanup traps. Source it, define a `cleanup` function, then call
# install_cleanup_traps.
#
# Why this exists instead of a bare `trap cleanup EXIT`:
#
#   bash runs an EXIT trap only for an exit the shell performs itself. A signal
#   the shell has no handler for kills it outright, and the EXIT trap never
#   runs. A CI cancellation (SIGTERM), a dropped connection (SIGHUP) or a
#   Ctrl-C (SIGINT) therefore skipped cleanup completely and left a VM running,
#   a cartridge attached and a volume mounted on a shared machine (#227).
#
# Contract for the sourcing script:
#
#   cleanup <status>   Runs once. It must be idempotent, it must not call
#                      `exit`, and it returns 0 when it left nothing behind or
#                      non-zero when a human must finish the job.
#
# On a signal the handler removes every trap, runs cleanup exactly once, then
# re-raises the signal with its default disposition. The parent (CI, make, a
# shell) therefore sees a genuine signal death with the conventional 128+n
# status rather than a fabricated exit code.
#
# The second half of the file solves the other half of the same problem: bash
# defers a trap until the current FOREGROUND command finishes, so a script that
# blocks for seven minutes in `br disk pack` swallows a SIGTERM for those seven
# minutes. run_interruptible runs such a command in the background and waits on
# it, because `wait` is interruptible. Without it the traps above are installed
# but arrive far too late to be useful.

# CLEANUP_CHILD_GRACE is how long an interrupted child gets to stop after
# SIGTERM before the reaper escalates.
CLEANUP_CHILD_GRACE_TRIES=10
CLEANUP_CHILD_GRACE_SLEEP=1

# CLEANUP_CHILD_PID names the child run_interruptible is waiting on, or "" when
# no long-running command is in flight. cleanup reads it through
# reap_interruptible_child.
CLEANUP_CHILD_PID=""

# _cleanup_done makes cleanup run-once. A signal handler runs cleanup and then
# re-raises, and the EXIT trap of a script that called `exit` runs it too, so
# the guard is what keeps a second pass from undoing the first one's work.
_cleanup_done=0

# run_cleanup_once calls the sourcing script's cleanup function at most once and
# passes the exit status through to it.
run_cleanup_once() {
  local status="${1:-0}"
  if [[ "$_cleanup_done" -eq 1 ]]; then
    return 0
  fi
  _cleanup_done=1
  cleanup "$status"
}

# on_exit_trap is the EXIT handler. It preserves the status the script was
# already exiting with, and promotes a clean exit to a failure when cleanup
# reports that it could not release everything.
on_exit_trap() {
  local rc="$1"
  run_cleanup_once "$rc" || rc=1
  exit "$rc"
}

# on_signal_trap is the INT/TERM/HUP handler. It disarms every trap first so the
# EXIT trap cannot run cleanup a second time, then re-raises the signal so the
# caller observes 128+n.
on_signal_trap() {
  local sig="$1"
  trap - EXIT INT TERM HUP
  printf '\n\033[1;31m==> interrupted by SIG%s — cleaning up before exit\033[0m\n' "$sig" >&2
  run_cleanup_once 1 || true
  kill -s "$sig" "$$"
}

# install_cleanup_traps arms all four dispositions. Call it after `cleanup` is
# defined and after every variable cleanup reads has a value, because a signal
# can arrive on the very next line.
install_cleanup_traps() {
  trap 'on_exit_trap $?' EXIT
  trap 'on_signal_trap INT' INT
  trap 'on_signal_trap TERM' TERM
  trap 'on_signal_trap HUP' HUP
}

# run_interruptible runs a long command in the background and waits for it,
# returning its exit status. The indirection is what lets a trap fire on time:
# bash will not run a trap while a foreground child is running, but it does
# interrupt `wait`.
run_interruptible() {
  local rc=0
  "$@" &
  CLEANUP_CHILD_PID=$!
  wait "$CLEANUP_CHILD_PID" || rc=$?
  CLEANUP_CHILD_PID=""
  return "$rc"
}

# reap_interruptible_child ends whatever run_interruptible was waiting on and
# reaps it EXACTLY once. Call it first thing in cleanup: an interrupted `wait`
# leaves the child running and unreaped, and a second `wait` on an already
# reaped pid is an error rather than a second reap, so both cases are absorbed
# here instead of in each caller.
reap_interruptible_child() {
  local pid="$CLEANUP_CHILD_PID" i
  CLEANUP_CHILD_PID=""
  if [[ -z "$pid" ]]; then
    return 0
  fi
  if kill -0 "$pid" 2>/dev/null; then
    kill -TERM "$pid" 2>/dev/null
    for ((i = 0; i < CLEANUP_CHILD_GRACE_TRIES; i++)); do
      kill -0 "$pid" 2>/dev/null || break
      sleep "$CLEANUP_CHILD_GRACE_SLEEP"
    done
    kill -0 "$pid" 2>/dev/null && kill -9 "$pid" 2>/dev/null
  fi
  # The child is a child of THIS shell (run_interruptible started it), so one
  # wait reaps it. A "not a child" error means it was already reaped.
  wait "$pid" 2>/dev/null
  return 0
}
