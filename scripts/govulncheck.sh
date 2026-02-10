#!/bin/sh
# Wrapper around govulncheck that suppresses known upstream vulnerabilities.
# Reads suppressed IDs from .govulncheckignore at the repo root.
# Runs a single JSON scan, filters suppressed IDs, and prints results.
set -e

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
REPO_ROOT=$(cd "$SCRIPT_DIR/.." && pwd)
IGNORE_FILE="$REPO_ROOT/.govulncheckignore"

command -v govulncheck >/dev/null 2>&1 || {
    echo "govulncheck not found; installing..."
    go install golang.org/x/vuln/cmd/govulncheck@latest
}

# Ensure govulncheck supports the current Go version. If it doesn't,
# it exits with a version mismatch error — upgrade and retry.
if ! govulncheck -version >/dev/null 2>&1; then
    echo "govulncheck does not support current Go version; upgrading..."
    go install golang.org/x/vuln/cmd/govulncheck@latest
fi

if [ ! -f "$IGNORE_FILE" ]; then
    echo "No .govulncheckignore found; running govulncheck without suppressions."
    exec govulncheck ./...
fi

# Load suppressed IDs (strip comments and blanks).
suppress=$(grep -v '^#' "$IGNORE_FILE" | grep -v '^$' | awk '{print $1}')

# Single JSON run — captures structured output for filtering.
json=$(govulncheck -json ./... 2>&1) || true

# Extract vulnerability IDs from finding objects.
found_ids=$(printf '%s\n' "$json" \
    | grep -o '"osv":"GO-[0-9]*-[0-9]*"' \
    | sed 's/"osv":"//;s/"//' \
    | sort -u)

if [ -z "$found_ids" ]; then
    echo "No vulnerabilities found."
    exit 0
fi

# Check each found ID against the suppress list.
unsuppressed=""
for id in $found_ids; do
    if echo "$suppress" | grep -qw "$id"; then
        echo "SUPPRESSED: $id"
    else
        unsuppressed="$unsuppressed $id"
    fi
done

if [ -n "$unsuppressed" ]; then
    echo ""
    echo "FAIL: unsuppressed vulnerabilities found:$unsuppressed"
    exit 1
fi

echo "All reported vulnerabilities are suppressed (known upstream issues)."
exit 0
