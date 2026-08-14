BINARY      := ai-reviewer
PKG         := github.com/sxwebdev/ai-reviewer
VERSION_PKG := $(PKG)/internal/version
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE        ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -s -w \
	-X $(VERSION_PKG).Version=$(VERSION) \
	-X $(VERSION_PKG).Commit=$(COMMIT) \
	-X $(VERSION_PKG).Date=$(DATE)

COMPOSE     := docker compose -f dev/docker/docker-compose.yml
PG_DSN      ?= postgres://ai_reviewer:ai_reviewer@localhost:5433/ai_reviewer?sslmode=disable

.PHONY: build install test test-db lint fmt tidy run start clean \
	infra-up infra-stop infra-down \
	migrateup migratedown migratecreate gensql

build:
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/$(BINARY)

install:
	go install ./cmd/ai-reviewer

# NOT the whole suite: without AI_REVIEWER_TEST_PG_DSN every database-backed
# test skips itself — 145 of 608 top-level tests, concentrated in service, jobs,
# store, migrator, app and cli. Green here means "the pure functions still
# work"; it says nothing about the review/findings/publish transaction, the
# claim CTE, the advisory locks or the migrations.
test:
	go test -race ./...

# The suite that counts. CI must run THIS target, with a PostgreSQL service
# container, not `make test`.
test-db:
	AI_REVIEWER_TEST_PG_DSN="$(PG_DSN)" go test -race ./...

lint:
	golangci-lint run ./...

fmt:
	go fix ./...
	gofmt -s -w .

tidy:
	go mod tidy

run: build
	./bin/$(BINARY) $(ARGS)

start: build
	./bin/$(BINARY) start

clean:
	rm -rf bin

# --- Local infrastructure ---------------------------------------------------

infra-up:
	$(COMPOSE) up -d --wait

infra-stop:
	$(COMPOSE) stop

# Also drops the data volume — use when the schema needs a clean slate.
infra-down:
	$(COMPOSE) down -v

# --- Database ---------------------------------------------------------------

migrateup:
	go run ./cmd/$(BINARY) migrations up --dsn "$(PG_DSN)"

migratedown:
	go run ./cmd/$(BINARY) migrations down --dsn "$(PG_DSN)"

migratecreate:
	go run ./cmd/$(BINARY) migrations create -p ./sql/migrations -name $(filter-out $@,$(MAKECMDGOALS))

gensql:
	pgxgen --config sql/pgxgen.yaml generate
