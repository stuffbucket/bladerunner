#!/usr/bin/env bash
# Minimal boot test - just boot Ubuntu without Incus to validate VM basics

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"

echo "==> Building bladerunner..."
cd "$PROJECT_ROOT"
make build sign

echo ""
echo "==> Minimal Boot Test"
echo "    This test boots Ubuntu with minimal cloud-init (no Incus)"
echo "    Goal: Validate that the VM boots and GUI shows console"
echo ""

# Clean up previous test
TEST_DIR=~/.bladerunner/minimal-test
rm -rf "$TEST_DIR"
mkdir -p "$TEST_DIR"

# Run with minimal setup
"$PROJECT_ROOT/bin/bladerunner" \
  --name minimal-test \
  --cpus 2 \
  --memory 2 \
  --disk-size 10 \
  --gui \
  --log-level debug

echo ""
echo "Test complete. Check:"
echo "  - Did GUI window appear?"
echo "  - Was console output visible in GUI?"
echo "  - Console log: $TEST_DIR/console.log"
echo "  - Main log: $TEST_DIR/bladerunner.log"
