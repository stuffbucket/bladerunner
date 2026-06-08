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
cd "$(dirname "$0")/.."

# Packages worth mutating: meaningful branching logic, fast deterministic tests.
# (cgo/darwin-only and thin-glue packages are intentionally excluded.)
PKGS=(timesource config disk oidc)

GB="$(go env GOPATH)/bin/gremlins"
if [ ! -x "$GB" ]; then
  echo "installing gremlins..."
  go install github.com/go-gremlins/gremlins/cmd/gremlins@latest
fi

# --timeout-coefficient 10: network-heavy packages (e.g. timesource, whose tests
#   dial/close a TCP listener) otherwise misclassify covered mutants as TIMED OUT
#   because socket teardown dominates the tiny baseline test duration. Raising the
#   per-mutant timeout ceiling reveals the true KILLED/LIVED result.
# --threshold-efficacy 90: efficacy = KILLED / (KILLED + LIVED). Exit non-zero if a
#   surviving mutant drops a package below 90% (all four are at 100% today).
fail=0
for p in "${PKGS[@]}"; do
  echo
  echo "=== mutation: internal/$p ==="
  if ! "$GB" unleash "./internal/$p" \
      --workers 2 --test-cpu 2 \
      --timeout-coefficient 10 \
      --threshold-efficacy 90; then
    echo "!! internal/$p below efficacy threshold (surviving mutants)" >&2
    fail=1
  fi
done

exit $fail
