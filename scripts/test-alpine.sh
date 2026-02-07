#!/usr/bin/env bash
# Test script to boot Alpine Linux with GUI to validate basic VM setup

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
BIN="${PROJECT_ROOT}/bin/bladerunner"

# Alpine Linux virtual image (already in raw format)
# Using 3.19 aarch64 virt image
ALPINE_URL="https://dl-cdn.alpinelinux.org/alpine/v3.19/releases/aarch64/alpine-virt-3.19.1-aarch64.iso"

echo "==> Building bladerunner..."
cd "$PROJECT_ROOT"
make build sign

echo ""
echo "==> Testing with Alpine Linux ISO"
echo "    This is a minimal test to validate:"
echo "    - EFI bootloader configuration"
echo "    - GUI window appears"
echo "    - Console output is visible"
echo "    - Basic VM lifecycle works"
echo ""
echo "    Note: Alpine ISO won't have cloud-init, so it will boot to login prompt"
echo "    Default login: root (no password)"
echo ""

# Clean up any previous test
rm -rf ~/.bladerunner/alpine-test

# Run with Alpine ISO instead of cloud image
# Note: This won't work directly because Alpine ISO needs different boot setup
# Instead, let's download a pre-installed Alpine disk image

echo ""
echo "ERROR: This test needs to be updated to use a proper Alpine disk image"
echo "Alpine ISOs require different boot configuration than what bladerunner expects"
echo ""
echo "Next steps:"
echo "1. Find/create an Alpine Linux raw disk image with EFI"
echo "2. Or create a minimal Ubuntu test without Incus"
echo "3. Validate GUI shows console output"

exit 1
