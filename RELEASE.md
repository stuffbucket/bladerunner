# Release Process

## Overview

bladerunner is **Apple Silicon only** due to its dependency on Apple's
Virtualization framework, which requires:

- Apple Silicon Mac (M1/M2/M3/M4)
- macOS 13+ (Ventura or later)
- CGO (not cross-compilable)
- Codesigning with entitlements

Releases are built and signed locally on an Apple Silicon Mac using
GoReleaser. Artifacts are uploaded to GitHub via `gh`. A GitHub Action
then automatically updates the Homebrew formula in the tap — no tokens
needed on your local machine.

## Prerequisites

- Apple Silicon Mac with Xcode Command Line Tools
- Push access to `stuffbucket/bladerunner`
- `gh` CLI authenticated with the `stuffbucket` account
- `HOMEBREW_TAP_TOKEN` secret configured in the repo (PAT with `repo`
  scope for `stuffbucket/homebrew-tap`) — used only by GitHub Actions
- Tools installed locally:

  ```bash
  make setup   # installs golangci-lint, goreleaser, govulncheck, trivy
  ```

## Release Steps

### 1. Prepare

```bash
git checkout main
git pull origin main
make check
```

### 2. Release

The `make release` target builds and signs locally, then uploads
artifacts to GitHub. A GitHub Action handles the Homebrew tap update.

```bash
make release TAG=v1.0.0
```

This does the following:

1. Tags the commit with the version
2. Builds an optimized arm64 binary with ldflags (via GoReleaser)
3. Codesigns the binary with Virtualization entitlements
4. Creates a tar.gz archive with checksums
5. Pushes the tag to origin
6. Creates a GitHub release with the archive attached (via `gh`)
7. **GitHub Action** generates and pushes the Homebrew formula to
   `stuffbucket/homebrew-tap` (triggered automatically)

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

The formula is pushed by the `release.yml` GitHub Action. Check the
Actions tab for errors. Ensure the `HOMEBREW_TAP_TOKEN` repo secret is
set to a PAT with `repo` scope for `stuffbucket/homebrew-tap`.

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
cd /tmp && git clone git@github.com-stuffbucket:stuffbucket/homebrew-tap.git
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
