# Release Process

## Overview

bladerunner is **Apple Silicon only** due to its dependency on Apple's
Virtualization framework, which requires:

- Apple Silicon Mac (M1/M2/M3/M4)
- macOS 13+ (Ventura or later)
- CGO (not cross-compilable)
- Codesigning with entitlements

Releases are built and signed locally on an Apple Silicon Mac, then published
to GitHub via the `gh` CLI. A GitHub Action automatically updates the Homebrew
formula when a release is published.

## Prerequisites

- Apple Silicon Mac with Xcode Command Line Tools
- Push access to `stuffbucket/bladerunner`
- `HOMEBREW_TAP_TOKEN` secret configured in repo settings (PAT with `repo` scope)
- Tools installed locally:

  ```bash
  make setup   # installs golangci-lint, goreleaser, configures git hooks
  ```

## Release Steps

### 1. Prepare

```bash
git checkout main
git pull origin main
make check
```

### 2. Release

The `make release` target builds, signs, tags, and publishes in one step:

```bash
make release TAG=v1.0.0
```

This does the following:

1. Builds an optimized arm64 binary via `scripts/build-macos.sh`
2. Signs the binary with Virtualization entitlements
3. Creates a tar.gz archive with checksums
4. Tags the commit and pushes the tag
5. Creates a GitHub release with the archive attached

The `release.yml` workflow then triggers automatically to update the Homebrew
formula in `stuffbucket/homebrew-tap`.

### 3. Verify

- **GitHub release**: <https://github.com/stuffbucket/bladerunner/releases>
- **Homebrew tap**: <https://github.com/stuffbucket/homebrew-tap>

Test installation:

```bash
brew uninstall bladerunner 2>/dev/null || true
brew install stuffbucket/tap/bladerunner
br --version
codesign --display --entitlements - $(which br)
```

## Checklist

- [ ] Tests pass (`make check`)
- [ ] Release published (`make release TAG=v1.0.0`)
- [ ] Homebrew formula updated (automatic via GitHub Action)
- [ ] `brew install` tested on Apple Silicon Mac

## Troubleshooting

### Homebrew formula not updated

The `release.yml` workflow triggers on `release: published`. If it didn't
run, trigger the manual workflow:

```bash
gh workflow run update-homebrew.yml -f version=v1.0.0
```

### Build fails

```bash
# Verify you're on Apple Silicon
uname -m   # should print arm64

# Test build locally
goreleaser release --snapshot --clean --skip=publish
```

### Rollback

```bash
# Delete release and tag
gh release delete v1.0.0 --yes
git tag -d v1.0.0
git push origin :refs/tags/v1.0.0

# Revert Homebrew tap
cd /tmp && git clone git@github.com:stuffbucket/homebrew-tap.git
cd homebrew-tap && git revert HEAD && git push
```

## Version Numbering

Semantic versioning:

- `v1.0.0` — major (breaking changes)
- `v1.1.0` — minor (new features, backward compatible)
- `v1.1.1` — patch (bug fixes)

## Commit Messages

Conventional commits for changelog generation:

- `feat: add bridged networking support`
- `fix: resolve memory leak in vm runtime`
- `docs: update installation instructions`

Git hooks enforce this format (after `make setup`).
