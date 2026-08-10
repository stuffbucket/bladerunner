# Contributing to bladerunner

## Quick Start

```bash
# Clone the repo
git clone https://github.com/stuffbucket/bladerunner.git
cd bladerunner

# Set up development environment (installs hooks + tools)
make setup

# Build and test
make build
make check
```

## Development Workflow

[AGENTS.md section 2](AGENTS.md#2-commands) is the authoritative command table
(`build`, `sign`, `check`, `test`, `test-linux`, `lint`, `security`, the smoke
targets, `clonedetect`) and the rules that go with them. `make help` lists every
target. The ones AGENTS.md does not cover:

```bash
make setup              # configure git hooks + install golangci-lint, goreleaser, govulncheck, trivy
make run ARGS='status'  # build, then run bin/br with those arguments
make fmt                # format Go sources with gofmt
make fmt-check          # check formatting without rewriting
make test-isolation     # prove the suite writes nothing outside its temp dirs
make lint-docs          # check docs against ASD-STE100 Simplified Technical English
make snapshot           # build a local goreleaser snapshot, no publish
```

Run `make check` before every commit: it is `fmt-check`, `vet`, `lint`,
`lint-linux` and `test`.

## Git Hooks

We use versioned git hooks in `.githooks/`. `make setup` points git at them:

```bash
git config core.hooksPath .githooks
```

- **pre-commit**: fast checks on staged Go files
- **commit-msg**: enforces the conventional-commit format
- **pre-push**: quick lint/build/test before pushing

Commit message rules live in
[AGENTS.md section 7](AGENTS.md#7-git-rules) — conventional commits, a subject
of 50 characters or fewer, no AI tool names, no emoji, no `--no-verify`.

## Project Structure

```text
cmd/bladerunner/    # CLI entry point and commands
internal/           # implementation packages
scripts/            # build, release and test scripts (see below)
test/e2e/           # opt-in, macOS-only end-to-end boot test
.github/workflows/  # CI/CD pipelines
```

Before you add a helper, read
[AGENTS.md section 3](AGENTS.md#3-owner-packages). It names the one package that
owns each cross-cutting operation (atomic writes, paths, boot stages, VM
lifecycle, DiskArbitration, port allocation, HTTP budgets). Call the owner
rather than writing a second copy, and run `make clonedetect` to check.

## Scripts

Every script in `scripts/` is supported, or it is not there. A script that
always fails is not validation. Nor is one you can only judge by eye. Such a
script is noise, so it is retired rather than kept.

| Script | Make target | Status | Needs |
|---|---|---|---|
| `smoke-cartridge.sh` | `make smoke-cartridge` | Supported | Apple Silicon Mac, codesigned binary, network. |
| `smoke-holder.sh` | `make smoke-holder` | Supported | Apple Silicon Mac, codesigned binary, network. |
| `test-cleanup-traps.sh` | `make test-traps` | Supported | Nothing. It runs anywhere, in seconds. |
| `test-isolation.sh` | `make test-isolation` | Supported | Nothing more than the Go toolchain. |
| `mutation-test.sh` | — | Supported, manual | gremlins. |
| `govulncheck.sh` | `make vulncheck` | Supported | Nothing more than the Go toolchain. |
| `diagnose-disk.sh` | — | Supported, manual | A disk image to examine. |
| `gen-brand-assets.sh` | — | Supported, manual | ImageMagick. |

Retired:

- `test-alpine.sh` and `test-minimal-boot.sh` (removed in #228). Neither could
  exercise the current build, and both asked a human to look at a GUI window
  instead of asserting a result. The gated end-to-end suite covers their
  scenario — see [docs/e2e-boot-smoke.md](docs/e2e-boot-smoke.md).

## Pull Requests

1. Create a feature branch from `main`
2. Make your changes with tests (see [AGENTS.md section 6](AGENTS.md#6-test-rules))
3. Ensure `make check` passes
4. Run `make test-linux` — a test that passes on macOS can still fail on CI's
   Linux runner
5. Open a PR with a clear description

`.github/workflows/ci.yml` then runs — on every pull request (no base-branch
filter, so stacked PRs are covered), on pushes to `main`, and weekly:

| Job | Runner | What it does |
|---|---|---|
| Build & Test | `ubuntu-latest` | `go build ./...`, `go test -race ./...`, coverage upload |
| Build & Test (macOS) | `macos-latest` | `go build ./...`, `go test -short -race ./...`, golangci-lint on the darwin build |
| Lint | `ubuntu-latest` | golangci-lint |
| Security Scan | `ubuntu-latest` | govulncheck + Trivy (SARIF to the Security tab) |

Every step of the macOS job is guarded on actually being on a macOS host, so it
**skips with a warning** rather than failing when a runner cannot be allocated.
A skipped job leaves the darwin half of the tree unverified for that commit —
read the annotation rather than the green tick.

## Code Style

Code, platform and test rules are in
[AGENTS.md sections 4–6](AGENTS.md#4-platform-rules). golangci-lint enforces
most of them; read `.golangci.yml` before disabling a rule.

## Testing on macOS

bladerunner needs macOS 13+ and Virtualization.framework. Anything that starts a
real VM needs a **codesigned** binary and real Apple Silicon hardware, so those
tests sit behind `testing.Short()` and never run in CI:

```bash
make sign               # a VM cannot start from an unsigned binary
make test               # unit tests
make smoke-cartridge    # live pack -> boot -> RW share -> ACPI eject
make smoke-holder       # live spawn -> kill the spawner -> VM survives -> drain
```

CI's macOS job runs `go test -short`, which covers every darwin unit test that
does not need a VM. The full boot path is covered by the opt-in end-to-end test
(see [docs/e2e-boot-smoke.md](docs/e2e-boot-smoke.md)), which runs locally or on
the self-hosted runner via the manually-dispatched `e2e-boot` workflow.

## Release Process

See [RELEASE.md](./RELEASE.md).

## Security

- Dependencies scanned weekly by Dependabot (`.github/dependabot.yml`)
- govulncheck + Trivy run in CI; `make security` runs both locally
- Security advisories at: <https://github.com/stuffbucket/bladerunner/security>

Report security issues to: <security@stuffbucket.dev>

## License

By contributing, you agree that your contributions will be licensed under the MIT License.
