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

.PHONY: help setup cache deps tidy fmt fmt-check vet test build build-release run sign check clean distclean lint

help: ## Show available targets
	@awk 'BEGIN {FS = ":.*## "}; /^[a-zA-Z0-9_.-]+:.*## / {printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

setup: ## First-time setup for contributors
	@echo "Setting up development environment..."
	@git config core.hooksPath .githooks
	@command -v golangci-lint >/dev/null 2>&1 || { echo "Installing golangci-lint..."; brew install golangci-lint; }
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

build: cache ## Build bladerunner binary
	@echo "Building $(APP_NAME)..."
	@$(GO_ENV) $(GO) build -o "$(BIN_PATH)" "$(CMD_PKG)"
	@echo "Built $(BIN_PATH)"

build-release: cache ## Build optimized release binary
	@echo "Building $(APP_NAME) (release)..."
	@$(GO_ENV) $(GO) build -trimpath -ldflags="-s -w" -o "$(BIN_PATH)" "$(CMD_PKG)"
	@echo "Built $(BIN_PATH)"

run: build ## Build and run (pass ARGS='...')
	@"$(BIN_PATH)" $(ARGS)

sign: build ## Codesign binary with virtualization entitlements
	@codesign --entitlements "$(ENTITLEMENTS)" -s "$(CODESIGN_IDENTITY)" "$(BIN_PATH)"
	@echo "Signed $(BIN_PATH) with $(ENTITLEMENTS)"

check: fmt-check vet lint test ## Run formatting check, vet, lint, and tests

lint: ## Run golangci-lint
	@command -v golangci-lint >/dev/null 2>&1 || { echo "Install: brew install golangci-lint"; exit 1; }
	@golangci-lint run

clean: ## Remove build outputs (preserves dependency cache)
	@rm -rf "$(BIN_DIR)"

distclean: clean ## Remove build outputs and Go build cache
	@rm -rf ./.cache
