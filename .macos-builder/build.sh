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

# The builder runner provisions bun + cargo, not Go (bladerunner is the first Go
# client). Use a Go already on PATH or in the usual spots (e.g. a `brew install
# go` on the mini); otherwise download the exact go.mod toolchain once into a
# persistent cache — $HOME survives across self-hosted runs, so this is a
# one-time ~60MB fetch, not per build.
ensure_go() {
  command -v go >/dev/null 2>&1 && return 0
  local d
  for d in /opt/homebrew/bin /usr/local/go/bin /usr/local/bin "$HOME/go/bin"; do
    if [ -x "$d/go" ]; then
      export PATH="$d:$PATH"
      return 0
    fi
  done
  local ver cache
  ver="$(awk '/^go / {print $2; exit}' go.mod)"
  cache="${RUNNER_TOOL_CACHE:-$HOME/.cache}/bladerunner-go/${ver}"
  if [ ! -x "${cache}/go/bin/go" ]; then
    echo "Provisioning Go ${ver} (darwin-arm64) into ${cache}"
    mkdir -p "${cache}"
    curl -fsSL "https://go.dev/dl/go${ver}.darwin-arm64.tar.gz" | tar -xz -C "${cache}"
  fi
  export PATH="${cache}/go/bin:$PATH"
  command -v go >/dev/null 2>&1
}
ensure_go || { echo "::error::could not provision a Go toolchain" >&2; exit 1; }
go version

echo "Building bladerunner ${TAG} (version ${VERSION})"

# Build the release binary.
go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o bin/br ./cmd/bladerunner

# Assemble the unsigned .app via br's own bundler.
rm -rf dist-dmg && mkdir -p dist-dmg
./bin/br menubar bundle --output dist-dmg
ls -la dist-dmg/Bladerunner.app/Contents

# Done — dist-dmg/Bladerunner.app (the config's app_path) is ready for the builder.
