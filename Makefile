# Romancy - Development Commands
# Run `make` or `make help` to see available commands

.PHONY: help build test test-coverage test-verbose lint lint-fix fmt tidy clean install-tools check build-examples build-cmd sync-schema check-schema-sync

# Default target - show help
.DEFAULT_GOAL := help

# Go parameters
GOCMD := go
GOBUILD := $(GOCMD) build
GOTEST := $(GOCMD) test
GOFMT := $(GOCMD) fmt
GOMOD := $(GOCMD) mod
GOCLEAN := $(GOCMD) clean

# golangci-lint
GOLANGCI_LINT_VERSION := v2.7.1
GOLANGCI_LINT := $(shell which golangci-lint 2>/dev/null || echo "$(shell go env GOPATH)/bin/golangci-lint")

help: ## Show this help message
	@echo "Romancy - Development Commands"
	@echo ""
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  %-18s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build the project
	$(GOBUILD) ./...

build-cmd: ## Build the CLI tool
	$(GOBUILD) -o romancy ./cmd/romancy

build-examples: ## Build all examples
	@for dir in examples/*/; do \
		echo "Building $$dir..."; \
		$(GOBUILD) -o $${dir}app $$dir || exit 1; \
	done

test: ## Run tests
	$(GOTEST) ./... -count=1

test-verbose: ## Run tests with verbose output
	$(GOTEST) ./... -v -count=1

test-coverage: ## Run tests with coverage
	$(GOTEST) ./... -cover -count=1

lint: ## Run golangci-lint
	$(GOLANGCI_LINT) run

lint-fix: ## Run golangci-lint with auto-fix
	$(GOLANGCI_LINT) run --fix

fmt: ## Format code
	$(GOFMT) ./...

tidy: ## Run go mod tidy
	$(GOMOD) tidy

clean: ## Clean build artifacts and caches
	$(GOCLEAN) ./...
	rm -rf romancy
	rm -f examples/*/app
	rm -f *.db *.db-shm *.db-wal
	rm -rf .golangci-lint-cache

install-tools: ## Install development tools (golangci-lint)
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

check: fmt lint test ## Run all checks (fmt, lint, test)

# Path of the canonical schema (durax-io/schema submodule, shared with edda/shikibu)
# and the //go:embed-target copy that ships in the Go module zip.
SCHEMA_SRC := schema/db/migrations
SCHEMA_DST := internal/migrations/sql

sync-schema: ## Refresh internal/migrations/sql/ from the schema/ submodule (run after bumping the submodule pointer)
	@if [ ! -f $(SCHEMA_SRC)/postgresql/*.sql ] && [ -z "$$(ls -A $(SCHEMA_SRC) 2>/dev/null)" ]; then \
		echo "schema/ submodule looks empty — run: git submodule update --init schema"; \
		exit 1; \
	fi
	@rm -rf $(SCHEMA_DST)
	@mkdir -p $(SCHEMA_DST)
	@cp -R $(SCHEMA_SRC)/. $(SCHEMA_DST)/
	@echo "synced $(SCHEMA_SRC) -> $(SCHEMA_DST)"

check-schema-sync: ## Fail if internal/migrations/sql/ has drifted from the schema/ submodule (CI guard)
	@if [ -z "$$(ls -A $(SCHEMA_SRC) 2>/dev/null)" ]; then \
		echo "schema/ submodule not initialized — run: git submodule update --init schema"; \
		exit 1; \
	fi
	@diff -ruN $(SCHEMA_SRC) $(SCHEMA_DST) >/dev/null || { \
		echo "ERROR: $(SCHEMA_DST) is out of sync with $(SCHEMA_SRC)."; \
		echo "       Run \`make sync-schema\` and commit the result."; \
		echo ""; \
		diff -ruN $(SCHEMA_SRC) $(SCHEMA_DST) || true; \
		exit 1; \
	}
	@echo "$(SCHEMA_DST) is in sync with $(SCHEMA_SRC)"
