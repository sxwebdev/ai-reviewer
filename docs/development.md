# Development

Requires **Go 1.26+**, `git`, Docker (for the dev database) and an
authenticated `claude` CLI for real reviews.

```bash
make infra-up      # PostgreSQL 18 in Docker on localhost:5433
make migrateup     # apply the schema to it
make build         # -> bin/ai-reviewer, version stamped via -ldflags
make test          # go test -race ./...
make test-db       # the same suite with the PostgreSQL-backed tests enabled
make lint          # golangci-lint run ./...
make fmt           # gofmt -s -w .
make gensql        # regenerate models/repositories with pgxgen
make infra-down    # stop and drop the volume
```

Database-backed tests skip themselves unless `AI_REVIEWER_TEST_PG_DSN` is set;
`make test-db` sets it for you. The DSN is used as an *admin* connection —
each test binary creates and migrates its own private database from it, so it
does not disturb the one `make migrateup` set up:

```bash
export AI_REVIEWER_TEST_PG_DSN="postgres://ai_reviewer:ai_reviewer@localhost:5433/ai_reviewer?sslmode=disable"
go test -race ./internal/store/...
```

**`make test` is not the suite — `make test-db` is.** Measured on this tree,
`go test -race ./...` runs 608 top-level tests and **145 of them skip** without a
DSN, concentrated exactly where the risk is: `internal/service` (65),
`internal/jobs` (35), `internal/store` (27), `internal/migrator` (8),
`internal/app` (4), `internal/cli` (3). Green on `make test` therefore means "the
pure functions still work" — the transaction that couples `mr_reviews`,
`mr_findings` and the `publish_review` enqueue, the claim CTE, the advisory
locks and the migrations are all untested in that run.

There is **no CI configuration in this repository**. Whatever you add must run
`make test-db` (with a PostgreSQL service container), not `make test`, plus
`make lint` and `gofmt -l .`. Recount the skips at any time with:

```bash
go test -race -count=1 -json ./... > /tmp/t.json
grep '"Action":"skip"' /tmp/t.json | grep -o '"Test":"[^"/]*"' | wc -l   # 145
grep -E '"Action":"(pass|fail|skip)"' /tmp/t.json | grep -o '"Test":"[^"/]*"' | wc -l  # 608
```

(The `[^"/]*` is what keeps subtests out of the count; the raw
`grep -c '"Action":"skip"'` reports 189, which is 145 top-level tests, 36
subtests and 8 packages that skipped whole.)

The engine, the services, the validator and the line mapper are all tested
against fake GitLab and LLM clients — no network, no `claude`. Keep new logic in
those layers pure so it stays fake-testable, and always run with `-race`.

`docs/competitive-analysis-*.md` and `docs/gopls-mcp-analysis.md` are kept as
analysis history; they describe evaluations, not the current architecture.

---

[← back to the README](../README.md)
