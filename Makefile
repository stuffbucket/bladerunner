SHELL := /bin/bash

.DEFAULT_GOAL := help

APP_NAME ?= br
CMD_PKG ?= ./cmd/bladerunner
BIN_DIR ?= ./bin
BIN_PATH ?= $(BIN_DIR)/$(APP_NAME)

ENTITLEMENTS ?= vz.entitlements
CODESIGN_IDENTITY ?= -

GO ?= go
GOPROXY ?= https://proxy.golang.org,direct
GOSUMDB ?= sum.golang.org
GOCACHE ?= $(CURDIR)/.cache/go-build
GO_ENV = GOCACHE="$(GOCACHE)" GOPROXY="$(GOPROXY)" GOSUMDB="$(GOSUMDB)"

# The Linux test container pins the same Go version as go.mod, which is what CI
# installs, and keeps its caches in named docker volumes of its own.
GO_VERSION ?= $(shell awk '/^go [0-9]/ {print $$2; exit}' go.mod)
LINUX_CACHE_VOL ?= bladerunner-linux-gocache
LINUX_MOD_VOL ?= bladerunner-linux-gomodcache

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  = -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: help setup cache deps tidy fmt fmt-check vet test test-linux test-isolation test-traps build build-release run sign check clean distclean lint lint-linux lint-docs vulncheck trivy security release snapshot smoke-cartridge smoke-holder clonedetect clonedetect-test

help: ## Show available targets
	@awk 'BEGIN {FS = ":.*## "}; /^[a-zA-Z0-9_.-]+:.*## / {printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

setup: ## First-time setup for contributors
	@echo "Setting up development environment..."
	@git config core.hooksPath .githooks
	@chmod +x .githooks/commit-msg .githooks/pre-push 2>/dev/null || true
	@command -v golangci-lint >/dev/null 2>&1 || { echo "Installing golangci-lint..."; brew install golangci-lint; }
	@command -v goreleaser >/dev/null 2>&1 || { echo "Installing goreleaser..."; brew install goreleaser; }
	@command -v govulncheck >/dev/null 2>&1 || { echo "Installing govulncheck..."; go install golang.org/x/vuln/cmd/govulncheck@latest; }
	@command -v trivy >/dev/null 2>&1 || { echo "Installing trivy..."; brew install trivy; }
	@echo "✓ Setup complete"

cache:
	@mkdir -p "$(GOCACHE)" "$(BIN_DIR)"

deps: cache ## Download and pre-build dependencies
	@$(GO_ENV) $(GO) mod download
	@$(GO_ENV) $(GO) build ./...

tidy: cache ## Run go mod tidy
	@$(GO_ENV) $(GO) mod tidy

fmt: ## Format Go sources
	@files="$$(find . -type f -name '*.go' -not -path './.cache/*')"; \
	if [ -n "$$files" ]; then \
		gofmt -w $$files; \
	fi

fmt-check: ## Check Go formatting
	@files="$$(find . -type f -name '*.go' -not -path './.cache/*')"; \
	if [ -z "$$files" ]; then \
		exit 0; \
	fi; \
	unformatted="$$(gofmt -l $$files)"; \
	if [ -n "$$unformatted" ]; then \
		echo "The following files are not gofmt formatted:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

vet: cache ## Run go vet
	@$(GO_ENV) $(GO) vet ./...

test: cache ## Run tests
	@$(GO_ENV) $(GO) test ./...

# test-linux runs the suite the way CI runs it: on Linux, against a READ-ONLY
# mount of the tree, with caches of its own so nothing the container builds
# reaches the host's GOCACHE (a darwin object file and a linux one must never
# share a cache). The Go version is read from go.mod, which is also what CI's
# setup-go reads, so the two cannot drift apart.
#
# CI runs linux/amd64 and a Mac runs this on linux/arm64. It therefore catches a
# broken build tag, a darwin-only assumption and platform logic. It does NOT
# catch behaviour that is specific to the amd64 architecture.
test-linux: ## Run the test suite in a Linux container (catches CI-only failures)
	@command -v docker >/dev/null 2>&1 || { echo "docker not found. Install: brew install colima docker && colima start"; exit 1; }
	@docker info >/dev/null 2>&1 || { echo "The Docker daemon is not running. Start it: colima start (or open Docker Desktop)"; exit 1; }
	@echo "Running tests on linux/$$(docker version --format '{{.Server.Arch}}') with go $(GO_VERSION)..."
	@docker run --rm \
		-v "$(CURDIR)":/src:ro -w /src \
		-v $(LINUX_CACHE_VOL):/tmp/gocache -v $(LINUX_MOD_VOL):/tmp/gomodcache \
		-e GOFLAGS=-mod=mod -e GOCACHE=/tmp/gocache -e GOMODCACHE=/tmp/gomodcache \
		"golang:$(GO_VERSION)" go test -race ./...

build: cache ## Build bladerunner binary
	@echo "Building $(APP_NAME)..."
	@$(GO_ENV) $(GO) build -ldflags="$(LDFLAGS)" -o "$(BIN_PATH)" "$(CMD_PKG)"
	@echo "Built $(BIN_PATH)"

build-release: cache ## Build optimized release binary
	@echo "Building $(APP_NAME) (release)..."
	@$(GO_ENV) $(GO) build -trimpath -ldflags="-s -w $(LDFLAGS)" -o "$(BIN_PATH)" "$(CMD_PKG)"
	@echo "Built $(BIN_PATH)"

run: build ## Build and run (pass ARGS='...')
	@"$(BIN_PATH)" $(ARGS)

sign: build ## Codesign binary with virtualization entitlements
	@codesign --entitlements "$(ENTITLEMENTS)" -s "$(CODESIGN_IDENTITY)" "$(BIN_PATH)"
	@echo "Signed $(BIN_PATH) with $(ENTITLEMENTS)"

smoke-cartridge: ## Live end-to-end cartridge smoke (pack -> boot -> RW share -> ACPI eject); needs codesign+network, ~15-25min
	@./scripts/smoke-cartridge.sh

smoke-holder: ## Live end-to-end holder smoke (spawn -> kill the spawner -> VM survives -> drain); needs codesign+network, ~5-15min
	@./scripts/smoke-holder.sh

# clonedetect lives in its own module under tools/, so the parent module's
# "./..." never loads it and `make check` is unaffected. Run it with -C rather
# than a package path for the same reason.
clonedetect: cache ## Rank duplicated concepts across packages (pass ARGS='-json')
	@$(GO_ENV) $(GO) -C tools/clonedetect run . -root ../.. $(ARGS)

clonedetect-test: cache ## Run the clonedetect tool's own tests
	@$(GO_ENV) $(GO) -C tools/clonedetect test ./...

check: fmt-check vet lint lint-linux test ## Run fast checks (format, vet, lint, test)

test-isolation: cache ## Prove the suite writes nothing outside its temp dirs
	@$(GO_ENV) ./scripts/test-isolation.sh

# No VM, no hardware: this signals a subject script that installs the same traps
# the smoke scripts install and asserts cleanup ran. Seconds, not minutes.
test-traps: ## Prove the smoke-test cleanup traps fire on EXIT, INT, TERM and HUP
	@./scripts/test-cleanup-traps.sh

lint: ## Run golangci-lint
	@command -v golangci-lint >/dev/null 2>&1 || { echo "Install: brew install golangci-lint"; exit 1; }
	@golangci-lint run

# On a Mac, `golangci-lint run` analyses the darwin build only, so nothing in a
# *_linux.go file is ever examined until CI does it. CI lints on Linux and fails
# there instead. Setting GOOS selects the other half of the tree, which is
# static analysis and needs no Linux host.
lint-linux: ## Run golangci-lint against the Linux build
	@command -v golangci-lint >/dev/null 2>&1 || { echo "Install: brew install golangci-lint"; exit 1; }
	@GOOS=linux golangci-lint run

# DOC_SOURCES is the prose the STE gate applies to. CHANGELOG.md is excluded:
# release-please generates it from commit subjects, so it is not hand-written
# prose and rewriting it would be overwritten on the next release.
DOC_SOURCES = README.md AGENTS.md CLAUDE.md CONTRIBUTING.md RELEASE.md docs/

lint-docs: ## Check docs against ASD-STE100 Simplified Technical English (errors only)
	@command -v vale >/dev/null 2>&1 || { echo "Install: brew install vale"; exit 1; }
	@out="$$(vale -min-severity error $(DOC_SOURCES) 2>&1)"; status=$$?; \
	printf '%s\n' "$$out" | grep -E ': error: ' || true; \
	printf '%s\n' "$$out" | tail -1; \
	exit $$status

vulncheck: ## Run govulncheck with suppression list
	@./scripts/govulncheck.sh

# trivy must gate on exactly what CI gates on, or `make security` goes red while
# the PR goes green and people learn to ignore it. CI excludes the same lockfile
# (.github/workflows/ci.yml, "Run Trivy vulnerability scanner"): ./site is a
# separate static sub-project whose npm deps are build-time tooling only, nothing
# ships at runtime, and pages.yml validates them on its own. The Go modules — the
# code that actually ships — are still scanned here.
trivy: ## Run Trivy filesystem vulnerability scan (same gate as CI)
	@command -v trivy >/dev/null 2>&1 || { echo "Install: brew install trivy"; exit 1; }
	@trivy fs --severity HIGH,CRITICAL --exit-code 1 --skip-dirs .cache,.git --skip-files site/package-lock.json .

security: vulncheck trivy ## Run all security scans (govulncheck + Trivy)

clean: ## Remove build outputs (preserves dependency cache)
	@rm -rf "$(BIN_DIR)"

distclean: clean ## Remove build outputs and Go build cache
	@rm -rf ./.cache

release: ## Build, sign, and publish a release
	@test -n "$(TAG)" || { echo "Usage: make release TAG=v1.0.0"; exit 1; }
	@command -v goreleaser >/dev/null 2>&1 || { echo "Install: brew install goreleaser"; exit 1; }
	@test "$$(uname -m)" = "arm64" || { echo "Error: releases must be built on Apple Silicon"; exit 1; }
	@git tag -a $(TAG) -m "Release $(TAG)" 2>/dev/null || true
	@goreleaser release --clean --skip=publish
	@git push origin $(TAG)
	@gh release create $(TAG) \
		dist/bladerunner_$${TAG#v}_darwin_aarch64.tar.gz \
		dist/checksums.txt \
		--generate-notes
	@echo ""
	@echo "Release $(TAG) published. Homebrew tap will be updated by GitHub Action."

snapshot: ## Build a local snapshot (no publish)
	@command -v goreleaser >/dev/null 2>&1 || { echo "Install: brew install goreleaser"; exit 1; }
	@goreleaser release --snapshot --clean --skip=publish
