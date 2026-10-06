BINARY        := blockbustr
CMD           := ./cmd/blockbustr
VERSION       ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# Tools fall back to `go install`'s location when they aren't on PATH.
SQLC ?= $(shell command -v sqlc 2>/dev/null || echo $$(go env GOPATH)/bin/sqlc)
GOLANGCI_LINT ?= $(shell command -v golangci-lint 2>/dev/null || echo $$(go env GOPATH)/bin/golangci-lint)

.PHONY: build run test test-integration vet lint generate sqlc-check dto dto-check fmt check test-scripts clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/$(BINARY) $(CMD)

run:
	go run $(CMD) -config config.yaml

test:
	go test ./...

vet:
	go vet ./...

lint:
	$(GOLANGCI_LINT) run

# Regenerate internal/store/pg/db from sqlc.yaml (queries + migrations).
generate:
	$(SQLC) generate

# Fails if the generated code doesn't match the queries/migrations.
sqlc-check:
	$(SQLC) diff
	$(SQLC) vet

# Jellyfin DTOs from the pinned openapi.json (oapi-codegen via `go tool`).
dto:
	python3 scripts/gen-dto.py

dto-check:
	python3 scripts/gen-dto.py --check

fmt:
	gofmt -w cmd internal

# Integration tests against the compose Postgres and Redis (deploy/docker-compose.yml).
# Each Postgres test creates and drops its own database; Redis tests use DB 15
# under a random key prefix and delete their keys afterwards.
TEST_DATABASE_URL ?= postgres://blockbustr:blockbustr@localhost:5432/postgres?sslmode=disable
TEST_REDIS_URL    ?= redis://localhost:6379/15
test-integration:
	BLOCKBUSTR_TEST_DATABASE_URL=$(TEST_DATABASE_URL) BLOCKBUSTR_TEST_REDIS_URL=$(TEST_REDIS_URL) \
		go test -count=1 ./internal/store/... ./internal/cache/... ./internal/auth/... ./internal/resolve/... ./internal/jfapi/handlers/... ./internal/library/... ./internal/metadata/...

# Definition of done (AGENTS.md): vet + generated code current + unit + integration tests + lint.
check: vet sqlc-check dto-check test test-integration lint

# Python tooling for recon fixtures (runs in the mitmproxy image).
test-scripts:
	scripts/capture/convert.sh --test

clean:
	rm -rf bin
