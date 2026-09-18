# Deal-Hunter — single static binary, zero external Go dependencies.
# CI runs here and on the office builder, not on GitHub Actions.
SHELL := /usr/bin/env bash
BIN     := dealhunter
PKG     := ./cmd/dealhunter
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILT   ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X github.com/xiabee/deal-hunter/internal/version.Version=$(VERSION) \
           -X github.com/xiabee/deal-hunter/internal/version.Commit=$(COMMIT) \
           -X github.com/xiabee/deal-hunter/internal/version.BuildDate=$(BUILT)

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_.-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: fmt
fmt: ## gofmt -w everything
	@gofmt -w cmd internal

.PHONY: vet
vet: ## go vet
	@go vet ./...

.PHONY: test
test: ## Unit + integration tests (offline fixtures)
	@go test ./...

.PHONY: race
race: ## Tests under the race detector
	@go test -race -count=1 ./...

.PHONY: scan
scan: ## Secret / private-topology scan (open-source gate)
	@go run $(PKG) secretscan -C .

.PHONY: ci
ci: ## Full local gate: fmt, vet, race tests, secret scan, cross-build
	@bash scripts/ci-local.sh

.PHONY: ci-quick
ci-quick: ## Fast gate (no race, no cross-build)
	@bash scripts/ci-local.sh --quick

.PHONY: ci-office
ci-office: ## Run the same gate on an office builder over SSH (DH_CI_HOST=...)
	@bash scripts/ci-office.sh

.PHONY: build
build: ## Build the native binary into dist/
	@mkdir -p dist
	@go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BIN) $(PKG)
	@./dist/$(BIN) version

.PHONY: build-linux
build-linux: ## Cross-compile the linux/amd64 deploy artifact (CGO off)
	@mkdir -p dist
	@CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BIN)-linux-amd64 $(PKG)
	@ls -lh dist/$(BIN)-linux-amd64

.PHONY: run
run: ## Run the daemon locally (scheduler + read-only API)
	@go run $(PKG) run

.PHONY: once
once: ## One collection round, then exit
	@go run $(PKG) once

.PHONY: doctor
doctor: ## Config, secret, directory and egress health check
	@go run $(PKG) doctor

.PHONY: probe
probe: ## Probe one source: make probe SOURCE=openrouter-free-models
	@go run $(PKG) probe -source $(SOURCE)

.PHONY: test-notify
test-notify: ## Send a self-test card to every configured channel
	@go run $(PKG) notify-test

.PHONY: clean
clean: ## Remove build output and local state
	@rm -rf dist data outreach
	@echo "cleaned"
