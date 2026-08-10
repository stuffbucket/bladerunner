# Release Process

Releases are automated. You do not build or tag one by hand.

bladerunner is **Apple Silicon only** — it depends on Apple's Virtualization
framework, so it needs CGO (not cross-compilable), macOS 13+, and codesigning
with entitlements.

## How a release happens

`.github/workflows/release-please.yml` runs on every push to `main` and drives
the whole pipeline:

1. **release-please** maintains `CHANGELOG.md` and the version, and opens a
   release PR. **Merging that PR** creates the `vX.Y.Z` tag and the GitHub
   Release. This is the only manual step.
2. **goreleaser** (`runs-on: macos-14`) builds the ad-hoc-signed `br`
   darwin/arm64 tarball plus `checksums.txt` and uploads them to the release.
   CGO links Virtualization.framework, so this must run on macOS.
3. **Homebrew sync** renders `packaging/homebrew/bladerunner.rb` with the
   version and tarball sha256 and pushes it to `stuffbucket/homebrew-tap`, so
   `brew upgrade` tracks every release. It is gated on the tarball having
   actually been uploaded.
4. **macos-build** (`.github/workflows/macos-build.yml`) dispatches to
   `stuffbucket/macos-builder`, a **private** repo that owns the only
   self-hosted macOS runner and every Apple signing secret. This repo holds
   neither. That builder produces the signed + notarized + stapled
   `Bladerunner.app` DMG and uploads it onto the release asynchronously.
5. **update-manifest** (`.github/workflows/publish-update-manifest.yml`) is
   dispatched to wait for the builder's `Bladerunner.app.tar.gz{,.sig}` upload,
   then deploys the site — the update manifest `br self-update` reads is derived
   state, built by `cmd/update-manifest` during the site build, and nothing
   commits it. If the assets do not appear within the poll window it publishes
   no manifest and says so with a `::notice::`.

Because step 4 is asynchronous and off-repo, the DMG lands on the release some
time after the tarball does. **The updater assets are not landing today** —
v0.4.7 shipped a DMG, a CLI tarball and checksums but no
`Bladerunner.app.tar.gz`, so there is no manifest and `br self-update` is
dormant.

## Do not use `make release`

The Makefile still carries a `release` target that tags, builds with goreleaser
and uploads via `gh` from a local Mac. It predates the pipeline above. Running
it pushes a tag that release-please did not create, which desynchronises
`CHANGELOG.md` and `.release-please-manifest.json`. Use it only to recover a
release the pipeline could not finish, and fix the manifest afterwards.

For a local build with no publishing, use `make snapshot`.

## Prerequisites (repository, not your machine)

- `HOMEBREW_TAP_TOKEN` — PAT with `repo` scope for `stuffbucket/homebrew-tap`
- `MACOS_BUILDER_PAT` — fine-grained PAT scoped to **only**
  `stuffbucket/macos-builder` with Actions: write. Until it is set, the DMG job
  safely no-ops (see the onboarding comment in `macos-build.yml`).

## Verify

- **GitHub release**: <https://github.com/stuffbucket/bladerunner/releases>
- **Homebrew tap**: <https://github.com/stuffbucket/homebrew-tap>

```bash
brew uninstall bladerunner 2>/dev/null || true
brew install stuffbucket/tap/bladerunner
br --version
codesign --display --entitlements - "$(which br)"
```

## Troubleshooting

### The release has no macOS CLI tarball

The goreleaser job **skips with a warning** rather than failing when its macOS
runner cannot be allocated, so a published release can end up without its
tarball (and, because the Homebrew job is gated on it, without a formula
update). Re-run that job against the existing tag. Look for the
`macOS release artifact NOT built` annotation on the run.

### Homebrew formula not updated

The formula is pushed by the `homebrew` job in `release-please.yml`. It only
runs when the goreleaser job reports `built=true`. Check that job first, then
that `HOMEBREW_TAP_TOKEN` is set.

### The DMG never appeared

That build is off-repo. Check `macos-build.yml` dispatched successfully, then
check `stuffbucket/macos-builder`.

### Rollback

```bash
gh release delete vX.Y.Z --yes
git push origin :refs/tags/vX.Y.Z
```

Then reset the version in `.release-please-manifest.json` so release-please
proposes the same version again.

## Version numbering

Semantic versioning, derived by release-please from conventional commit messages
(`release-please-config.json`). While the project is pre-1.0,
`bump-minor-pre-major` and `bump-patch-for-minor-pre-major` are both set, so a
breaking change bumps the **minor** and both `feat:` and `fix:` bump the
**patch**. See [AGENTS.md section 7](AGENTS.md#7-git-rules) for the commit
rules; git hooks enforce the format after `make setup`.
