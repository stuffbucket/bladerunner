#!/bin/bash
# Build and sign macOS arm64 binary for release.
# Usage: ./scripts/build-macos.sh <version>
#
# Must run on an Apple Silicon Mac.

set -euo pipefail

VERSION="${1:-}"
if [[ -z "$VERSION" ]]; then
  echo "Usage: $0 <version>" >&2
  echo "Example: $0 v1.0.0" >&2
  exit 1
fi

VERSION_CLEAN="${VERSION#v}"
PROJECT="bladerunner"
BINARY="br"

COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS="-s -w -X main.version=${VERSION_CLEAN} -X main.commit=${COMMIT} -X main.date=${DATE}"

# Verify we're on macOS arm64
if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "Error: Must run on macOS" >&2
  exit 1
fi

if [[ "$(uname -m)" != "arm64" ]]; then
  echo "Error: Must run on Apple Silicon (arm64)" >&2
  exit 1
fi

echo "Building ${PROJECT} ${VERSION} (${COMMIT})"
echo ""

# Clean
rm -rf build
mkdir -p build/staging

# Build
echo "==> Building darwin/arm64..."
CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build \
  -trimpath \
  -ldflags="${LDFLAGS}" \
  -o "build/staging/${BINARY}" \
  ./cmd/bladerunner

# Sign
echo "==> Signing with entitlements..."
codesign --entitlements vz.entitlements -s - -f "build/staging/${BINARY}"
codesign --verify --verbose "build/staging/${BINARY}"

# Package
cp README.md vz.entitlements build/staging/

echo "==> Creating archive..."
tar -czf "build/${PROJECT}_${VERSION_CLEAN}_darwin_aarch64.tar.gz" \
  -C build/staging .

# Checksums
echo "==> Generating checksums..."
cd build
shasum -a 256 "${PROJECT}_${VERSION_CLEAN}_darwin_aarch64.tar.gz" > checksums.txt
cd ..

echo ""
echo "✓ Build complete!"
echo ""
echo "Archive:  build/${PROJECT}_${VERSION_CLEAN}_darwin_aarch64.tar.gz"
echo "Checksum: build/checksums.txt"
cat build/checksums.txt
echo ""
echo "Next: make release TAG=${VERSION}  (or manually: gh release create ...)"
