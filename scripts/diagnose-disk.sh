#!/usr/bin/env bash
# Diagnose the disk image to understand its structure

set -euo pipefail

DISK="${1:-$HOME/.bladerunner/incus-vm/base-image.raw}"

if [ ! -f "$DISK" ]; then
    echo "Error: Disk not found: $DISK"
    exit 1
fi

echo "==> Disk Image Analysis: $DISK"
echo ""

echo "File size:"
ls -lh "$DISK"
echo ""

echo "File type:"
file "$DISK"
echo ""

echo "First 512 bytes (hex):"
hexdump -C "$DISK" | head -20
echo ""

echo "Looking for partitions..."
if command -v gdisk >/dev/null 2>&1; then
    echo "Using gdisk to inspect GPT:"
    echo "i" | sudo gdisk "$DISK" 2>/dev/null || echo "gdisk failed"
else
    echo "gdisk not installed (brew install gptfdisk)"
fi
echo ""

echo "Looking for filesystem signatures..."
strings "$DISK" | grep -E "(GRUB|grub|EFI|efi|vmlinuz|initrd)" | head -20 || true
echo ""

echo "Checking for EFI partition (usually at start of disk)..."
dd if="$DISK" bs=1M count=50 2>/dev/null | strings | grep -iE "(efi|boot|grub)" | head -10 || true
