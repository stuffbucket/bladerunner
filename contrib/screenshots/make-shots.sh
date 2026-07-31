#!/usr/bin/env bash
# Regenerate contrib/assets/ from the recorded sessions in caps/.
#
# Every frame comes from a real run captured with script(1), so the colour is
# the CLI's own rather than a reconstruction. Where a frame is cropped, BR_ROWS
# narrows the window onto rows the terminal genuinely showed — nothing is
# re-typed, re-staged, or hand-edited. The one addition is the `$ br ...` prompt
# line, which restates the command so a reader need not infer it from the title.
#
# Rendering goes to build/ (untracked) and only the optimised PNGs are copied
# into ../assets, so the committed files stay small and the intermediate SVGs
# stay out of the tree.
set -euo pipefail
cd "$(dirname "$0")"

BUILD=build
ASSETS=../assets
mkdir -p "$BUILD" "$ASSETS"

R="python3 render.py"

BR_CMD="br --help" BR_ROWS=0:14 $R caps/05-help.raw $BUILD/01-what-it-is.png \
  "br --help" \
  "An Incus VM on macOS, driven straight from Apple Virtualization.framework." \
  "No Lima, no Colima, no Docker Desktop — one binary owns the whole lifecycle."

BR_CMD="br up" BR_ROWS=0:17 $R caps/80-up-fixed.raw $BUILD/02-booting.png \
  "br up  —  booting" \
  "Boot is staged and observable. Each stage reports itself and the guest console" \
  "is tailed live, so a hang shows you where it stopped instead of just hanging."

BR_CMD="br up" BR_ROWS=17:30 $R caps/80-up-fixed.raw $BUILD/03-vm-ready.png \
  "br up  —  ready" \
  "SSH and the Incus API are forwarded to localhost over virtio-vsock. A holder" \
  "process owns the VM, so it keeps running after the command that started it exits."

BR_CMD="br status" BR_ROWS=0:23 $R caps/81-status.raw $BUILD/04-status.png \
  "br status" \
  "What is running, which guest image it booted, and where the disk, logs and" \
  "cloud-init live on the host."

BR_CMD="br shell -- 'whoami; incus --version; uname -srm'" \
  $R caps/86-shell.raw $BUILD/05-shell.png \
  "br shell" \
  "A shell in the guest, over the vsock-forwarded SSH. Incus is already running" \
  "there — the VM arrives provisioned rather than needing to be set up."

BR_CMD="br ls" $R caps/82-ls.raw $BUILD/06-instances.png \
  "br ls" \
  "Incus instances inside the VM. This Alpine container survived a full VM" \
  "restart and came back with its address — the VM is a real Incus host."

BR_CMD="br exec demo -- /bin/sh -c 'cat /etc/alpine-release; uname -m'" \
  $R caps/83-exec.raw $BUILD/07-exec.png \
  "br exec demo -- ..." \
  "Running commands inside a container in the VM, from the macOS shell." \
  "aarch64 throughout: the guest and its containers are native, not emulated."

BR_CMD="br stop" $R caps/87-stop.raw $BUILD/08-stop.png \
  "br stop" \
  "A clean shutdown. The guest is asked to power off over ACPI and the disk is" \
  "synced before detach, so the image is never torn away mid-write."

# Palette-reduce before committing. Terminal output is mostly flat colour, so
# 256 colours is visually identical here — including the gradient banner — at
# a little under half the bytes. Lossless recompression alone gains nothing,
# because rsvg already emits tightly compressed PNGs.
echo
for f in "$BUILD"/*.png; do
    name="$(basename "$f")"
    magick "$f" -strip -colors 256 -define png:compression-level=9 "$ASSETS/$name"
    printf '%-30s %6.1f KB\n' "assets/$name" \
        "$(echo "$(stat -f%z "$ASSETS/$name")/1024" | bc -l)"
done
