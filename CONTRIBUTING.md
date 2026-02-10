# Contributing to bladerunner

## Quick Start

```bash
# Clone the repo
git clone https://github.com/stuffbucket/bladerunner.git
cd bladerunner

# Set up development environment (installs hooks + tools)
make setup

# Build and test
make all
```

## Development Workflow

### Building

```bash
make build      # Build binary to ./bin/br
make sign       # Build and sign with entitlements
make run ARGS='--gui=false'  # Build, sign, and run
```

### Testing

```bash
make test       # Full test suite with race detector
make check      # Run formatting check, vet, lint, and tests
```

### Linting

```bash
make lint       # Run golangci-lint
make fmt        # Format code with gofmt
make fmt-check  # Check Go formatting
```

## Git Hooks

We use versioned git hooks in `.githooks/`. The `make setup` command configures git to use them automatically:

```bash
git config core.hooksPath .githooks
```

### Hooks Included

- **commit-msg**: Enforces conventional commit format
- **pre-push**: Runs quick lint/build/test before pushing

### Commit Message Format

```text
type(scope)?: description (50 chars max)

[optional body]

[optional footer]
```

Types: `feat`, `fix`, `refactor`, `test`, `build`, `chore`, `docs`, `perf`, `ci`

Examples:

```text
feat(vm): add bridged networking support
fix(cli): resolve memory leak in status command
docs: update installation instructions
```

## Project Structure

```text
cmd/bladerunner/    # CLI entry point and commands
internal/
  boot/             # VM boot and console management
  config/           # Configuration management
  control/          # Control socket and IPC
  incus/            # Incus client
  logging/          # Structured logging with rotation
  provision/        # Cloud-init provisioning
  report/           # Startup report generation
  ssh/              # SSH key management
  ui/               # Terminal UI theming
  util/             # Utility functions
  vm/               # VM configuration and runtime (VZ framework)
scripts/            # Build and release scripts
.github/workflows/  # CI/CD pipelines
```

## Pull Requests

1. Create a feature branch from `main`
2. Make your changes with tests (if applicable)
3. Ensure `make check` passes
4. Open a PR with a clear description

CI will automatically:

- Run tests with race detector
- Lint with golangci-lint (comprehensive checks)
- Scan for vulnerabilities (govulncheck + trivy)

## Release Process

See [RELEASE.md](./RELEASE.md) for detailed release instructions.

Quick summary:

```bash
# Build, sign, tag, and publish (requires Apple Silicon Mac)
make release TAG=v1.0.0
```

GitHub Actions will:

1. Update the Homebrew formula in `stuffbucket/homebrew-tap`

## Code Style

- Follow standard Go conventions
- Run `make fmt` before committing
- golangci-lint enforces comprehensive checks (see `.golangci.yml`)
- Keep functions focused and testable
- Document exported functions and types
- Use conventional commit messages

## Testing on macOS

bladerunner requires macOS 13+ and the Virtualization framework. Tests that interact with VZ must run on macOS:

```bash
# Run on macOS only
make test
```

CI runs on `macos-latest` for this reason.

## Security

- All dependencies scanned weekly by Dependabot
- govulncheck runs on every push
- Trivy scans for vulnerabilities
- Security advisories at: <https://github.com/stuffbucket/bladerunner/security>

Report security issues to: <security@stuffbucket.dev>

## License

By contributing, you agree that your contributions will be licensed under the MIT License.
