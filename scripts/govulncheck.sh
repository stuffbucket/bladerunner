#!/bin/sh
# Wrapper around govulncheck that suppresses known upstream vulnerabilities.
# Uses JSON output for reliable parsing. The human-readable scan is printed
# first so findings remain visible in logs — only the exit code is masked
# for suppressed IDs.
#
# Whenever an entry is added here, include the date and a short rationale.
set -e

# Suppressed vulnerability IDs (one per line, comments allowed).
SUPPRESS="
GO-2026-4357  # 2026-02-09 upstream dep, no fix available
GO-2026-4359  # 2026-02-09 upstream dep, no fix available
"

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
    if echo "$SUPPRESS" | grep -qw "$id"; then
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
