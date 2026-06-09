#!/usr/bin/env bash
set -euo pipefail

# Bladerunner producer for stuffbucket/macos-builder.
# Builds the `br` binary and assembles the unsigned Bladerunner.app, leaving it
# at the config's app_path (dist-dmg/Bladerunner.app). It does NOT sign, build a
# dmg, notarize, or staple — the builder owns that tail (top-level sign with the
# virtualization entitlement + dmg + notarize + staple + sha256).
#
# Bladerunner.app has a single executable (the `br` binary), so no inside-out
# pre-signing is needed: the builder's plain top-level sign covers everything.
#
# Builder-supplied env consumed: TAG.

VERSION="${TAG#v}"

echo "Building bladerunner ${TAG} (version ${VERSION})"

# Build the release binary (flags mirror release-macos-dmg.yml).
go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o bin/br ./cmd/bladerunner

# Assemble the unsigned .app via br's own bundler.
rm -rf dist-dmg && mkdir -p dist-dmg
./bin/br menubar bundle --output dist-dmg
ls -la dist-dmg/Bladerunner.app/Contents

# Done — dist-dmg/Bladerunner.app (the config's app_path) is ready for the builder.
