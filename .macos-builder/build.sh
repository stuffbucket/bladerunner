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

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=.macos-builder/go-toolchain.sh
source "${SCRIPT_DIR}/go-toolchain.sh"

GO_PLATFORM="${GO_PLATFORM:-darwin-arm64}"
GO_BASE_URL="${GO_BASE_URL:-https://go.dev/dl}"

# The builder runner provisions bun + cargo, not Go (bladerunner is the first Go
# client). Use a Go already on PATH or in the usual spots (e.g. a `brew install
# go` on the mini); otherwise download the exact go.mod toolchain, verified
# against the digest pinned in go-toolchain.sha256.
ensure_go() {
  command -v go >/dev/null 2>&1 && return 0
  local d
  for d in /opt/homebrew/bin /usr/local/go/bin /usr/local/bin "$HOME/go/bin"; do
    if [ -x "$d/go" ]; then
      export PATH="$d:$PATH"
      return 0
    fi
  done

  local ver bin
  ver="$(awk '/^go / {print $2; exit}' go.mod)"
  bin="$(go_provision "${ver}" \
    "${RUNNER_TOOL_CACHE:-$HOME/.cache}/bladerunner-go" \
    "${SCRIPT_DIR}/go-toolchain.sha256" \
    "${GO_BASE_URL}" \
    "${GO_PLATFORM}")" || return 1
  export PATH="${bin}:$PATH"
  command -v go >/dev/null 2>&1
}
ensure_go || { echo "::error::could not provision a Go toolchain" >&2; exit 1; }

# Release provenance: which compiler built the signed app, and where it came
# from. A toolchain already present on the runner is recorded as such — it is
# outside this script's control, and saying so is more useful than implying it
# was verified here.
echo "toolchain: $(go version)"
echo "toolchain-path: $(command -v go)"
echo "toolchain-source: ${GO_TOOLCHAIN_SOURCE:-preinstalled on the runner}"
echo "toolchain-digest: ${GO_TOOLCHAIN_DIGEST:-not applicable (not downloaded by this script)}"

echo "Building bladerunner ${TAG} (version ${VERSION})"

# Build the release binary.
go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o bin/br ./cmd/bladerunner

# Assemble the unsigned .app via br's own bundler.
rm -rf dist-dmg && mkdir -p dist-dmg
./bin/br menubar bundle --output dist-dmg
ls -la dist-dmg/Bladerunner.app/Contents

# Done — dist-dmg/Bladerunner.app (the config's app_path) is ready for the builder.
