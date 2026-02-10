#!/bin/sh
# Wrapper around govulncheck that suppresses known upstream vulnerabilities.
# Reads suppressed IDs from .govulncheckignore at the repo root.
# Uses JSON output for reliable parsing. The human-readable scan is printed
# first so findings remain visible in logs — only the exit code is masked
# for suppressed IDs.
set -e

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
REPO_ROOT=$(cd "$SCRIPT_DIR/.." && pwd)
IGNORE_FILE="$REPO_ROOT/.govulncheckignore"

if [ ! -f "$IGNORE_FILE" ]; then
    echo "No .govulncheckignore file found; running govulncheck without suppressions."
    exec govulncheck ./...
fi

# Load suppressed IDs (strip comments and blanks).
suppress=$(grep -v '^#' "$IGNORE_FILE" | grep -v '^$' | awk '{print $1}')

command -v govulncheck >/dev/null 2>&1 || {
    echo "govulncheck not found; installing..."
    go install golang.org/x/vuln/cmd/govulncheck@latest
}

# Run human-readable scan first so the full output appears in logs.
govulncheck ./... 2>&1 || true

# Run again with JSON for structured parsing.
json=$(govulncheck -json ./... 2>/dev/null) || true

# Extract vulnerability IDs from JSON finding objects.
# Each finding has an osv field with the ID.
found_ids=$(printf '%s\n' "$json" \
    | grep -o '"osv":"GO-[0-9]*-[0-9]*"' \
    | sed 's/"osv":"//;s/"//' \
    | sort -u)

if [ -z "$found_ids" ]; then
    echo ""
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

echo ""
echo "All reported vulnerabilities are suppressed (known upstream issues)."
exit 0
