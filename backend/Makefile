# VORTECH backend developer commands. Run `make help` for the list.

SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

# Load local configuration (never committed) and export it to commands.
-include .env
export

MODULE        := github.com/vortech/backend
VERSION       ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT        ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
IMAGE         ?= vortech/backend
BIN_DIR       := bin

GO_LDFLAGS := -s -w \
	-X $(MODULE)/internal/buildinfo.Version=$(VERSION) \
	-X $(MODULE)/internal/buildinfo.Commit=$(COMMIT) \
	-X $(MODULE)/internal/buildinfo.BuildTime=$(BUILD_TIME)

# Pinned tool versions, executed with `go run` so nothing is installed globally.
STATICCHECK_VERSION := v0.8.1
GOVULNCHECK_VERSION := v1.8.0
SQLC_VERSION        := 1.30.0

# Prefer a local sqlc binary of any version; otherwise use the pinned image.
SQLC ?= $(shell command -v sqlc 2>/dev/null || echo "docker run --rm -u $$(id -u):$$(id -g) -v $(CURDIR):/src -w /src sqlc/sqlc:$(SQLC_VERSION)")

COMPOSE := docker compose -f deployments/local/compose.yaml

# Integration tests create and drop their own database through this
# maintenance connection.
TEST_DATABASE_URL ?= postgres://vortech:$(or $(POSTGRES_PASSWORD),vortech_dev_only)@127.0.0.1:$(or $(POSTGRES_HOST_PORT),55432)/postgres?sslmode=disable

##@ Help
.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n"} \
		/^[a-zA-Z0-9_-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Setup
.PHONY: env
env: ## Create .env from .env.example if missing
	@test -f .env && echo ".env already exists" || (cp .env.example .env && echo "created .env")

.PHONY: tidy
tidy: ## Tidy and verify go.mod/go.sum
	go mod tidy
	go mod verify

##@ Quality
.PHONY: fmt
fmt: ## Format all Go code
	gofmt -s -w .

.PHONY: fmt-check
fmt-check: ## Fail if any Go file is not gofmt-formatted
	@out="$$(gofmt -s -l .)"; if [ -n "$$out" ]; then echo "unformatted files:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet: ## Run go vet (including integration-tagged files)
	go vet ./...
	go vet -tags integration ./tests/...

.PHONY: lint
lint: ## Run staticcheck
	go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...
	go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) -tags integration ./tests/...

.PHONY: vuln
vuln: ## Scan dependencies for known vulnerabilities
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

.PHONY: test
test: ## Run unit tests with the race detector
	go test -race -count=1 ./...

.PHONY: test-integration
test-integration: ## Run integration tests against the compose PostgreSQL (make db-up first)
	TEST_DATABASE_URL='$(TEST_DATABASE_URL)' go test -race -count=1 -tags integration ./tests/integration/...

.PHONY: cover
cover: ## Unit test coverage report (coverage.html)
	go test -count=1 -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1
	go tool cover -html=coverage.out -o coverage.html

.PHONY: check
check: fmt-check vet lint test sqlc-check ## Everything CI runs except integration tests and vuln scan

##@ Code generation
.PHONY: sqlc
sqlc: ## Regenerate the sqlc query layer
	$(SQLC) generate

.PHONY: sqlc-check
sqlc-check: ## Fail if generated sqlc code is stale
	$(SQLC) vet
	$(SQLC) diff

##@ Build & run
.PHONY: build
build: ## Build api, worker and migrate into ./bin
	CGO_ENABLED=0 go build -trimpath -ldflags "$(GO_LDFLAGS)" -o $(BIN_DIR)/ ./cmd/api ./cmd/worker ./cmd/migrate

.PHONY: run-api
run-api: ## Run the API on the host (uses .env)
	go run ./cmd/api

.PHONY: run-worker
run-worker: ## Run the worker on the host (uses .env)
	go run ./cmd/worker

##@ Database
.PHONY: db-up
db-up: ## Start local PostgreSQL and wait until healthy
	$(COMPOSE) up -d --wait postgres

.PHONY: db-down
db-down: ## Stop local PostgreSQL (data kept)
	$(COMPOSE) stop postgres

.PHONY: db-reset
db-reset: ## Destroy local PostgreSQL data and start fresh
	$(COMPOSE) down -v --remove-orphans
	$(COMPOSE) up -d --wait postgres

.PHONY: db-psql
db-psql: ## Open psql in the local PostgreSQL container
	$(COMPOSE) exec postgres psql -U vortech -d vortech

.PHONY: migrate-up
migrate-up: ## Apply pending migrations
	go run ./cmd/migrate up

.PHONY: migrate-down
migrate-down: ## Roll back the most recent migration
	go run ./cmd/migrate down

.PHONY: migrate-status
migrate-status: ## Show migration status
	go run ./cmd/migrate status

.PHONY: migrate-create
migrate-create: ## Create a migration: make migrate-create NAME=add_players
	@test -n "$(NAME)" || (echo "usage: make migrate-create NAME=snake_case_name"; exit 1)
	go run ./cmd/migrate create $(NAME)

##@ Containers
.PHONY: docker-build
docker-build: ## Build the versioned container image
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

.PHONY: compose-up
compose-up: ## Run postgres + migrate + api + worker in containers
	$(COMPOSE) --profile app up -d --build --wait

.PHONY: compose-down
compose-down: ## Stop the full container stack (data kept)
	$(COMPOSE) --profile app down

.PHONY: compose-logs
compose-logs: ## Follow container logs
	$(COMPOSE) --profile app logs -f

.PHONY: clean
clean: ## Remove build and coverage artifacts
	rm -rf $(BIN_DIR) coverage.out coverage.html
