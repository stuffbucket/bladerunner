#!/usr/bin/env bash
# mutation-test.sh — gremlins mutation testing on bladerunner's high-value, pure
# (non-cgo, deterministic) packages. Mutation testing flips conditionals/operators
# and deletes statements, then checks whether a test FAILS ("kills" the mutant).
# Surviving (LIVED) mutants reveal weak assertions that line coverage can't.
#
# Run locally:  ./scripts/mutation-test.sh
# CI runs this nightly + on-demand (.github/workflows/mutation.yml); it is NOT a
# blocking PR check.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1

# Packages worth mutating: meaningful branching logic, fast deterministic tests.
# (cgo/darwin-only and thin-glue packages are intentionally excluded.)
#
# vmhost is included despite linking objc transitively (via internal/vm) because
# it holds the unmount-veto decision logic, where an untested branch means the
# cartridge silently loses crash protection. It needs a higher timeout
# coefficient than the rest — see TIMEOUT_FOR below.
PKGS=(timesource config disk oidc portalloc instance bootstage imagebuild vmhost)

# timeout_for echoes the --timeout-coefficient for a package. The default of 10
# is enough for the pure packages; vmhost links objc, so its baseline test run is
# dominated by link/startup cost and every mutant is misclassified TIMED OUT at
# 10. Measured: at 20 it reports Killed 84 / Lived 0 / efficacy 100%.
timeout_for() {
  case "$1" in
    vmhost) echo 20 ;;
    *)      echo 10 ;;
  esac
}

GB="$(go env GOPATH)/bin/gremlins"
if [ ! -x "$GB" ]; then
  echo "installing gremlins..."
  go install github.com/go-gremlins/gremlins/cmd/gremlins@latest
fi

# --timeout-coefficient: network-heavy packages (e.g. timesource, whose tests
#   dial/close a TCP listener) and objc-linking ones (vmhost) otherwise
#   misclassify covered mutants as TIMED OUT because teardown/link cost dominates
#   the tiny baseline test duration. Raising the per-mutant timeout ceiling
#   reveals the true KILLED/LIVED result. See timeout_for above.
# --threshold-efficacy 90: efficacy = KILLED / (KILLED + LIVED). Exit non-zero if a
#   surviving mutant drops a package below 90% (all eight are at 100% today).
#   NOTE: efficacy ignores NOT COVERED mutants, so a high score does not mean the
#   package is well covered — vmhost is at 100% efficacy but only ~53% mutator
#   coverage, because Host.Run's steps need a real VM. Read both numbers.
fail=0
for p in "${PKGS[@]}"; do
  echo
  echo "=== mutation: internal/$p ==="
  if ! "$GB" unleash "./internal/$p" \
      --workers 2 --test-cpu 2 \
      --timeout-coefficient "$(timeout_for "$p")" \
      --threshold-efficacy 90; then
    echo "!! internal/$p below efficacy threshold (surviving mutants)" >&2
    fail=1
  fi
done

exit $fail
