# MyJobFyt development tasks.
#
# `make help` lists everything. `make check` is what CI runs and what must pass before
# a commit.

SHELL := /bin/bash
.DEFAULT_GOAL := help

BACKEND  := backend
FRONTEND := frontend
COMPOSE  := docker compose

# Load .env for targets that talk to the local stack, without leaking it into targets
# that do not need it.
define load_env
	set -a; [ -f .env ] && . ./.env; set +a;
endef

.PHONY: help
help: ## List available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| sort \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

# --- environment ------------------------------------------------------------

.PHONY: env
env: ## Create .env from .env.example if absent
	@if [ -f .env ]; then echo ".env already exists; leaving it alone"; \
	else cp .env.example .env && echo "created .env from .env.example"; fi

.PHONY: keys
keys: ## Generate a development RS256 keypair and print the env lines to paste
	@openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out /tmp/myjobfyt-dev.key 2>/dev/null
	@openssl rsa -in /tmp/myjobfyt-dev.key -pubout -out /tmp/myjobfyt-dev.pub 2>/dev/null
	@echo "# Paste these into .env (development only — production keys come from a secret manager):"
	@echo "AUTH_JWT_PRIVATE_KEY_PEM=\"$$(awk '{printf "%s\\n", $$0}' /tmp/myjobfyt-dev.key)\""
	@echo "AUTH_JWT_PUBLIC_KEY_PEM=\"$$(awk '{printf "%s\\n", $$0}' /tmp/myjobfyt-dev.pub)\""
	@rm -f /tmp/myjobfyt-dev.key /tmp/myjobfyt-dev.pub

.PHONY: up
up: ## Start the local dependency stack (Postgres, Redis, NATS, MinIO, Mailpit)
	$(COMPOSE) up -d postgres redis nats minio mailpit
	@echo "waiting for health..."
	@for i in $$(seq 1 40); do \
		unhealthy=$$($(COMPOSE) ps --format '{{.Health}}' | grep -cv healthy || true); \
		if [ "$$unhealthy" = "0" ]; then echo "all services healthy"; exit 0; fi; \
		sleep 2; \
	done; \
	echo "services did not become healthy in time"; $(COMPOSE) ps; exit 1

.PHONY: up-full
up-full: ## Start the stack including ClamAV and the observability profile
	$(COMPOSE) --profile full --profile observability up -d

.PHONY: down
down: ## Stop the stack, keeping volumes
	$(COMPOSE) down

.PHONY: clean
clean: ## Stop the stack and DELETE all local data volumes
	$(COMPOSE) down -v

.PHONY: logs
logs: ## Tail dependency logs
	$(COMPOSE) logs -f --tail=100

.PHONY: dev
dev: up migrate ## Start dependencies and apply migrations
	@echo ""
	@echo "ready:"
	@echo "  Postgres   localhost:15432   (myjobfyt/myjobfyt)"
	@echo "  Redis      localhost:16379"
	@echo "  NATS       localhost:14222   (monitoring http://localhost:18222)"
	@echo "  MinIO      localhost:19000   (console http://localhost:19001)"
	@echo "  Mailpit    smtp :11025       (ui http://localhost:18025)"
	@echo ""
	@echo "next: make run-api"

# --- database ---------------------------------------------------------------

.PHONY: migrate
migrate: ## Apply pending migrations
	@$(load_env) cd $(BACKEND) && go run ./cmd/migrate up

.PHONY: migrate-down
migrate-down: ## Roll back the most recent migration
	@$(load_env) cd $(BACKEND) && go run ./cmd/migrate down

.PHONY: migrate-status
migrate-status: ## Show migration status
	@$(load_env) cd $(BACKEND) && go run ./cmd/migrate status

.PHONY: migrate-validate
migrate-validate: ## Verify every migration parses and is reversible (no database needed)
	@cd $(BACKEND) && go run ./cmd/migrate validate

.PHONY: migrate-reset
migrate-reset: ## Roll every migration back, then re-apply (development only)
	@$(load_env) cd $(BACKEND) && go run ./cmd/migrate reset && go run ./cmd/migrate up

.PHONY: psql
psql: ## Open a psql shell on the development database
	$(COMPOSE) exec postgres psql -U myjobfyt -d myjobfyt

# --- backend ----------------------------------------------------------------

.PHONY: run-api
run-api: ## Run the API server
	@$(load_env) cd $(BACKEND) && go run ./cmd/api

.PHONY: run-worker
run-worker: ## Run the event worker
	@$(load_env) cd $(BACKEND) && go run ./cmd/worker

.PHONY: run-scheduler
run-scheduler: ## Run the scheduler
	@$(load_env) cd $(BACKEND) && go run ./cmd/scheduler

.PHONY: build
build: ## Build all backend binaries into backend/bin
	cd $(BACKEND) && go build -trimpath -ldflags "-s -w" -o bin/ ./cmd/...

.PHONY: fmt
fmt: ## Format Go code
	cd $(BACKEND) && gofmt -w . && go mod tidy

.PHONY: fmt-check
fmt-check: ## Fail if any Go file is unformatted
	@cd $(BACKEND) && out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then echo "unformatted files:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet: ## Run go vet
	cd $(BACKEND) && go vet ./...

.PHONY: lint
lint: ## Run golangci-lint if installed
	@if command -v golangci-lint >/dev/null 2>&1; then \
		cd $(BACKEND) && golangci-lint run ./...; \
	else \
		echo "golangci-lint not installed; skipping (install: https://golangci-lint.run/usage/install/)"; \
	fi

.PHONY: test
test: ## Run backend unit tests
	cd $(BACKEND) && go test -race ./...

.PHONY: test-integration
test-integration: ## Run integration tests against the local stack
	@$(load_env) cd $(BACKEND) && go test -race -tags=integration -count=1 ./...

.PHONY: cover
cover: ## Run unit tests with a coverage report
	cd $(BACKEND) && go test -race -coverprofile=coverage.out -covermode=atomic ./... \
		&& go tool cover -func=coverage.out | tail -1

# --- frontend ---------------------------------------------------------------

.PHONY: fe-install
fe-install: ## Install frontend dependencies
	cd $(FRONTEND) && npm ci || npm install

.PHONY: fe-dev
fe-dev: ## Run the frontend development server
	cd $(FRONTEND) && npm run dev

.PHONY: fe-build
fe-build: ## Build the frontend
	cd $(FRONTEND) && npm run build

.PHONY: fe-lint
fe-lint: ## Lint the frontend
	cd $(FRONTEND) && npm run lint

.PHONY: fe-typecheck
fe-typecheck: ## Type-check the frontend
	cd $(FRONTEND) && npm run typecheck

.PHONY: fe-test
fe-test: ## Run frontend tests
	cd $(FRONTEND) && npm test

# --- aggregate --------------------------------------------------------------

.PHONY: check
check: fmt-check vet test migrate-validate ## Everything CI runs; must pass before a commit
	@echo ""
	@echo "all checks passed"
