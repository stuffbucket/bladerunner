#!/usr/bin/env bash
# gen-brand-assets.sh — regenerate the binary brand assets from the SVG masters.
#
# Source of truth: assets/brand/*.svg (hand-designed, version-controlled).
# Outputs (checked in, because CI has no rsvg-convert/iconutil):
#   cmd/bladerunner/assets/AppIcon.icns   <- assets/brand/appicon.svg   (dock icon)
#   cmd/bladerunner/assets/menubar-b.png  <- assets/brand/menubar-b.svg (menu-bar glyph, 2x)
#
# Both outputs are go:embed'd by the bladerunner binary. Re-run this after
# editing either SVG master, then commit the regenerated outputs.
#
# Requires (macOS): rsvg-convert (brew install librsvg) and iconutil (built in).
set -euo pipefail
cd "$(dirname "$0")/.."

command -v rsvg-convert >/dev/null || { echo "need rsvg-convert (brew install librsvg)" >&2; exit 1; }
command -v iconutil >/dev/null || { echo "need iconutil (macOS)" >&2; exit 1; }

SRC_APP=assets/brand/appicon.svg
SRC_MENU=assets/brand/menubar-b.svg
OUT_DIR=cmd/bladerunner/assets
mkdir -p "$OUT_DIR"

# --- AppIcon.icns: macOS iconset (each logical size + its @2x) -> icns --------
ICONSET="$(mktemp -d)/AppIcon.iconset"
mkdir -p "$ICONSET"
emit() { rsvg-convert -w "$1" -h "$1" "$SRC_APP" -o "$ICONSET/$2"; }
emit 16   icon_16x16.png
emit 32   icon_16x16@2x.png
emit 32   icon_32x32.png
emit 64   icon_32x32@2x.png
emit 128  icon_128x128.png
emit 256  icon_128x128@2x.png
emit 256  icon_256x256.png
emit 512  icon_256x256@2x.png
emit 512  icon_512x512.png
emit 1024 icon_512x512@2x.png
iconutil -c icns "$ICONSET" -o "$OUT_DIR/AppIcon.icns"
echo "wrote $OUT_DIR/AppIcon.icns"

# --- menu-bar glyph: 44x44 (2x of the 22pt status item) alpha mask -----------
# Black on transparent; the Go menubar tints the alpha by VM state.
rsvg-convert -w 44 -h 44 "$SRC_MENU" -o "$OUT_DIR/menubar-b.png"
echo "wrote $OUT_DIR/menubar-b.png"
