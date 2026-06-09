# frozen_string_literal: true

# Homebrew CASK for the signed Bladerunner.app menubar app — SOURCE OF TRUTH.
#
# The release DMG workflow (.github/workflows/release-macos-dmg.yml) fills the
# version + DMG sha256 and syncs a rendered copy into stuffbucket/homebrew-tap
# (Casks/bladerunner-app.rb). Named "bladerunner-app" so it does not collide
# with the CLI formula "bladerunner" in the same tap.
#
#   brew install --cask stuffbucket/tap/bladerunner-app   # the menubar .app
#   brew install        stuffbucket/tap/bladerunner        # the `br` CLI
cask "bladerunner-app" do
  version "PLACEHOLDER_VERSION"
  sha256 "PLACEHOLDER_SHA256_DMG"

  url "https://github.com/stuffbucket/bladerunner/releases/download/v#{version}/Bladerunner-#{version}.dmg"
  name "Bladerunner"
  desc "Menubar app for the bladerunner Incus VM runner"
  homepage "https://github.com/stuffbucket/bladerunner"

  depends_on macos: ">= :ventura"
  depends_on arch: :arm64

  app "Bladerunner.app"

  caveats <<~EOS
    Bladerunner.app is a menubar app (no dock icon) and bundles the `br` CLI.
    For just the standalone CLI: brew install stuffbucket/tap/bladerunner
  EOS
end
