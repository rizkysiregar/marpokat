# Developer entry points. Tools that are not Go (sqlc, golangci-lint) run in Docker, so only Go and Docker
# are needed locally. Local settings come from .env (copy .env.example).

SQLC_IMAGE   := sqlc/sqlc:1.29.0
LINT_IMAGE   := golangci/golangci-lint:v2.14.0
VERSION      ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo dev)
LDFLAGS      := -s -w -X main.version=$(VERSION) -X main.commit=$(VERSION)

ifneq (,$(wildcard .env))
include .env
export
endif

.PHONY: help
help: ## list targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-16s %s\n", $$1, $$2}'

.PHONY: test
test: ## unit tests (no external dependencies)
	go test ./...

.PHONY: test-race
test-race: ## unit tests with the race detector
	go test -race ./...

.PHONY: vet
vet: ## go vet + gofmt check
	go vet ./...
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)

.PHONY: lint
lint: ## golangci-lint (Docker)
	docker run --rm -v "$(CURDIR)":/src -w /src $(LINT_IMAGE) golangci-lint run ./...

.PHONY: gen
gen: ## generate internal/store from queries/*.sql (sqlc in Docker)
	docker run --rm -v "$(CURDIR)":/src -w /src $(SQLC_IMAGE) generate

.PHONY: gen-check
gen-check: gen ## fail if generated code is out of date
	@git diff --exit-code -- internal/store || (echo "internal/store is stale: run make gen and commit"; exit 1)

.PHONY: build
build: ## build bin/marpokat
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/marpokat ./cmd/marpokat

.PHONY: deps-up
deps-up: ## start Postgres, pgbouncer, Redis, RabbitMQ, MinIO
	docker compose up -d

.PHONY: deps-down
deps-down: ## stop local dependencies (data is kept)
	docker compose down

.PHONY: deps-reset
deps-reset: ## stop local dependencies and delete their data
	docker compose down -v

.PHONY: migrate
migrate: ## apply migrations to the local database
	go run ./cmd/marpokat migrate up

.PHONY: migrate-down
migrate-down: ## roll back the latest migration (local only)
	go run ./cmd/marpokat migrate down

.PHONY: migrate-status
migrate-status: ## list migrations and whether they ran
	go run ./cmd/marpokat migrate status

.PHONY: run-api
run-api: ## run the API (public :8080, management :9090)
	go run ./cmd/marpokat api

.PHONY: run-worker
run-worker: ## run the worker (management :9091)
	MANAGEMENT_ADDR=:9091 go run ./cmd/marpokat worker

.PHONY: docker
docker: ## build the container image locally
	docker build --build-arg VERSION=$(VERSION) -t marpokat-server:$(VERSION) .
