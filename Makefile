# Romancy - Development Commands
# Run `make` or `make help` to see available commands

.PHONY: help build test test-coverage test-verbose lint lint-fix fmt tidy clean install-tools check build-examples build-cmd

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
