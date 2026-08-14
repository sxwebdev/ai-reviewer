# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A self-hosted GitLab review agent for engineering teams. It scans the open merge
requests of configured repositories, reviews each one with the Claude Code CLI
when its head SHA changes, publishes validated findings as inline discussions
from a service account, classifies human-review and author-action state, and
sends a per-team Slack digest at 09:00 and 16:30 Europe/Moscow.

It runs as N replicas on PostgreSQL + [River](https://riverqueue.com) +
[mx](https://github.com/tkcrm/mx). There is no web UI, no manual
approve/reject/publish step, and no "MRs where I am the reviewer" — all of that
belonged to the personal tool this repository used to be.
`docs/team-service-redesign-plan.md` is the design plan (Russian, authoritative
for intent; **the code wins on facts**, and the plan carries dated correction
notes where the two diverged).

Requires Go 1.26+, `git`, PostgreSQL ≥ 16 and an authenticated `claude` CLI.

## Commands

```bash
make build          # -> bin/ai-reviewer, version stamped via -ldflags
make test           # go test -race ./...   (see the warning below)
make test-db        # the same suite with AI_REVIEWER_TEST_PG_DSN set
make lint           # golangci-lint run ./...
make fmt            # go fix ./... && gofmt -s -w .
make tidy           # go mod tidy
make gensql         # pgxgen: regenerate internal/models + internal/store/repos

make infra-up       # PostgreSQL 18 in Docker on localhost:5433
make infra-stop     # stop it, keep the volume
make infra-down     # stop it and DROP the volume
make migrateup      # migrations up against the dev database
make migratecreate  # migrations create -name <name>

go test -race ./internal/review/...                 # one package
go test -race -run TestValidate ./internal/review   # one test
```

Runtime: `ai-reviewer {start,scan,review,digest,doctor,migrations}`.

- `start` — the production process: Postgres, River workers, periodic jobs, the
  mx ops server (`/livez`, `/readyz`, `/metrics` on 10000; `/debug/pprof` only
  when `ops.profiler.enabled`, which nothing authenticates, hence opt-in).
- `scan`, `digest`, `review` **enqueue a River job and print its id**; a running
  `start` executes it. `review --local` is the exception — it runs the worker's
  code in the CLI process.
- `doctor [--local]` checks config, git, `claude` auth under the configured
  mode, timezone, Postgres, pending migrations, River tables, GitLab auth and
  every repository, `head_pipeline` visibility, Slack membership per channel,
  workdir writability. Non-zero exit on any failure. Run it after config changes.
- `<ref>` accepts a full MR URL, `group/sub/repo!123` or `project-id:iid`
  (`internal/gitlab/ref.go`).

## Local development loop

```bash
make infra-up && make migrateup
make test-db
```

**`make test` is not the suite — `make test-db` is.** Measured on this tree:
625 top-level tests, of which **150 skip** without `AI_REVIEWER_TEST_PG_DSN`,
concentrated exactly where the risk lives — `internal/service` 66,
`internal/jobs` 36, `internal/store` 28, `internal/migrator` 8, `internal/app` 6,
`internal/cli` 3. A green `make test` means "the pure functions still work" and
says nothing about the single-commit transaction, the claim CTE, the advisory
locks or the migrations. There is **no CI in this repository**; whatever gets
added must run `make test-db` with a PostgreSQL service container.

The DSN is an **admin** connection: each test binary creates and migrates its own
private database from it and truncates app tables per test, so packages running
in parallel do not share state.

Dev runs **PostgreSQL 18**, production targets **17**, the floor is **16**. That
spread is deliberate: it exercises the version-dependent path in `0001_init`,
whose `uuidv7()` shim is installed **only when `pg_proc` says the server lacks
the built-in**, so exactly one `uuidv7()` exists anywhere. The shim reproduces
the built-in's sub-millisecond `rand_a` precision, so id ordering does not differ
between dev and production. Do not write PG18-only SQL.

The whole schema is **one migration**, `0001_init`; nothing was ever deployed
from an intermediate state. Everything from here gets its own numbered pair —
do not amend `0001_init`.

## Architecture

```text
GitLab (truth about MRs) ──REST v4 + GraphQL──┐
                                              ▼
   cli ──► app ──► jobs (River) ──► service ──► {store, gitlab, slack, match,
                                                 git, llm, review, domain}
                        │                                      │
                        ▼                                      ▼
                   PostgreSQL                              security
```

**Dependencies point strictly downward** and `domain` imports nothing internal at
all. Never invert an edge; if a lower layer needs something from a higher one,
declare the interface on the lower side (`internal/jobs/deps.go` is the worked
example — it is why `internal/service` never imports River).

- **`internal/cli`** — urfave/cli tree. Flags, config load, dispatch. The CLI
  enqueues; it does not do the work.
- **`internal/app`** — composition root. `runtime.go` wires everything,
  `start.go` registers it with the mx launcher, `doctor*.go` the checks,
  `adapters.go` maps `service` types onto the interfaces `jobs` declares.
- **`internal/jobs`** — River. Seven kinds in two chains:
  `scan → scan_repo → {review → publish_review, publish_review sweep}` and
  `digest → slack_send`, plus `cleanup`. Owns queues, uniqueness, attempt
  budgets, timeouts, the periodic schedule and the drain.
- **`internal/service`** — one operation end to end: `ScanRepository`,
  `RunReview`, `PublishReview`, `BuildDigest`, `SendMessage`. The only layer that
  touches GitLab, Slack, the engine and Postgres in the same breath, and the
  owner of every wire→domain mapping.
- **`internal/domain`** — pure classifiers over `MergeRequestSnapshot`. No I/O,
  no config, no logging.
- **`internal/review`** — the engine: fan-out passes → cross-pass dedupe →
  validator → skeptic → deterministic verifiers → rank and cap, plus the risk,
  completeness and coverage reports. Rewiring its data sources means changing who
  fills `review.ReviewInput`, not the engine.

## Invariants — do not weaken these

- **Go owns validation and positions, not the model.**
  `internal/review/validator.go` + `line_mapper.go` own the file-in-diff check,
  line→GitLab-position mapping (added→`new_line`, removed→`old_line`,
  context→both), the fallback ladder (exact line → nearest changed line →
  overview note), severity threshold, dedupe, secret scrubbing, body cap and
  max-comments cap. The LLM supplies a file, a line and a side — never a SHA,
  never a final position.
- **Claude never writes anywhere.** `permission_mode: dontAsk` plus an allowlist
  that is read-only **and** worktree-scoped: `Read/Grep/Glob(${worktree}/**)`
  with `llm.WorktreePlaceholder` substituted per review. No `Edit`/`Write`, and
  **no `Bash` rule of any kind** — an allow rule is a prefix match, and every git
  subcommand taking diff options accepts `--output=<path>` (arbitrary write) and
  `--no-index <file>` (arbitrary read), both reproduced against the real CLI.
  Pinned by `TestDefaultAllowedToolsAreReadOnlyAndWorktreeScoped`.
- **The `claude` subprocess environment is an allowlist, never `os.Environ()`.**
  This process holds the PAT, the Slack token and the database password, and the
  subprocess reads attacker-authored merge requests. `internal/security` plus
  `internal/llm/auth.go` own the list. A strip list cannot work — the names that
  matter are the `AI_REVIEWER_*` ones nobody enumerates.
- **Deterministic Claude auth.** `llm.claude.auth.mode` decides which credential
  the subprocess sees and *actively removes* the other, along with
  `CLAUDE_CODE_USE_BEDROCK`, `CLAUDE_CODE_USE_VERTEX`, `ANTHROPIC_AUTH_TOKEN` and
  `ANTHROPIC_BASE_URL` unless `extra_env` opts back in. Never pass `--bare`: it
  skips the keychain and hard-requires `ANTHROPIC_API_KEY`.
- **Never** approve or merge an MR; never resolve or delete other people's
  discussions; never change reviewers, labels, title or description.
- **Findings only on changed lines.** A finding whose file is not in the MR diff
  is dropped.
- **Dedup by fingerprint** — `review.Fingerprint(projectID, mrIID, file,
  category, title)`, sha256, head-SHA-independent. This is the dedupe contract in
  Postgres *and* in the GitLab markers: do not change its inputs or format.
- **Binary / vendored / generated files never reach the LLM**, nor do paths
  matching `review.ignore_globs`. All enforced in one place — `parseDiffs`
  (`internal/service/diff.go`) — because that is the only chokepoint that also
  removes the file from the finding-eligible set: an excluded path must be unable
  to *receive* a comment, not merely be absent from a prompt. `ignore_globs` is
  checked on **both sides of a rename**.
- **GitLab is the source of truth about merge requests; Postgres holds this
  service's operational state.** Never mirror GitLab into Postgres.
- **Team isolation.** A repository belongs to exactly one team (validated
  fail-fast, the error naming both). One team's failure must not affect another's
  digest.
- **The success marker is written last.** Findings first, each `note_id` recorded
  immediately after its POST; then the summary note carrying the review marker;
  then `status='succeeded'`. Nothing may be posted after the summary, so
  `succeeded` can never be a false success.
- **All deferred work is a River job** — publishing, Slack delivery, worktree
  sweeping. Never "a goroutine in the background": a goroutine dies with its
  replica and leaves no trace. The flip side: a job's own *reads* stay inside it;
  do not create a job per network call.
- **No secrets in logs.** Resolved secrets go through `security.RegisterSecret`
  at config load; the redacting zapcore masks messages, string fields and
  `Any`-rendered values.

### Removed — do not reintroduce

Manual approval before publication; per-user draft notes; SQLite (with FTS5 and
`internal/index`); `scope=reviews_for_me`; the web UI and its `sync`/`daemon`
commands.

## Conventions

- **Concurrency is River's job** for everything that goes through the queue —
  unique jobs with an explicit in-flight `ByState` set, plus leader-only periodic
  jobs. **Do not add an application-level lock for what the queue already
  covers.** There are exactly three Postgres advisory locks, all for work the
  queue cannot see:
  1. **One review of one head SHA** — `review --local` runs outside the queue, so
     the CLI *and* the `review` worker both take
     `hashtext("review:<project>:<iid>:<sha>")`. A one-sided lock would exclude
     only a second `--local`, which is precisely not the case it promises.
  2. **The migration sequence** (`App.Migrate`) — one lock held across *both*
     migrators, because River's takes none of its own. This is what makes
     `postgres.migrate_on_start` safe **on by default**: every replica migrates
     itself on the way up and all but the first find nothing pending. Weaken it
     and a fresh install with two replicas has both running `CREATE TABLE
     river_job`. Verified with three replicas against an empty database: exactly
     one applied.
  3. **One repository's git mirror** — an in-process semaphore, then the injected
     `git.Locker` (`runtime.go` passes `jobs.NewLocker(pool)`, a blocking
     `pg_advisory_lock`). Cross-replica, so a shared `review.workdir` does not
     corrupt a mirror. Order is fixed review → mirror, so no deadlock. **Not
     covered:** `cleanup` takes neither lock before `os.RemoveAll` — safe only
     because it is leader-only and its TTLs (6h worktrees, 30d mirrors) dwarf any
     job's lifetime.
- **Jobs.** `review` has `MaxAttempts = 1` (a failed review already burned
  tokens); `publish_review` and `slack_send` get 10, being cheap network retries.
  Uniqueness always restricts `ByState` to in-flight states — River's default
  includes `Completed`, which would make a re-review of the same SHA silently
  vanish. `river:"unique"` tags do nothing without `UniqueOpts.ByArgs = true`.
  Check `InsertResult.UniqueSkippedAsDuplicate` rather than assuming an insert
  enqueued. Per-SHA failures back off 15m / 1h / 6h / stop until a new head SHA
  resets the count; the attempt row is written **before** the expensive work, so
  an OOM inside the LLM pass is countable even though River never re-runs it, and
  a review killed by **our own shutdown** discards its row (four deploys must not
  blacklist a healthy SHA) while a job *timeout* does count.
- **Transactions.** `service.RunReview` takes an `OnPersist` callback that runs
  *inside* the transaction writing `mr_reviews` + `mr_findings`; `internal/jobs`
  passes a closure calling `river.InsertTx`. That single commit is what makes
  "reviewed but nobody will publish it" unrepresentable. A nil `OnPersist` is the
  dry-run path.
- **Idempotency.** `PublishReview` posts only findings with `note_id IS NULL`,
  adopts comments already carrying a matching fingerprint marker, and skips a
  summary already present for the head SHA. `SendMessage` claims the row and
  branches on the status it had *before* the claim; a part stranded in `sending`
  is resent on purpose (a duplicate is noise, a lost digest is silence) and
  counted by `slack_resend_uncertain_total`. That counter must keep meaning "a
  human may see this twice": an answer that *refuses* delivery hands the claim
  back, and only a transport failure or 5xx leaves the row at `sending`.
- **Config.** Sources: YAML → env (`AI_REVIEWER_*`) → Vault. Two rules that break
  things silently if dropped. (1) **Defaults come from `DefaultConfig()`, not
  struct tags** — the defaults plugin fills zero values after the file load and
  would flip an explicit `false` back to `true`; pinned by
  `TestLoadPreservesExplicitFalse`, and the consequence is that embedded
  third-party defaults (`logger.Config`, `ops.Config`) must be populated
  explicitly. (2) **Every externally-settable field needs an explicit `env:`
  tag** — otherwise xconfig word-splits the field path and `gitlab.token` becomes
  `AI_REVIEWER_GIT_LAB_TOKEN`; pinned by `TestEnvNamesRoundTrip`. The Vault key is
  the field's full env name, prefix included. Leaf fields inside `teams[]`
  deliberately carry no `env:` tag: inside a slice element the name is built from
  the expanded path (`AI_REVIEWER_TEAMS_0_NAME`).
- **Database.** Not-found is `pgx.ErrNoRows`; there is no sentinel-error package.
  The zero `dbtypes.JSON` marshals to SQL NULL while every `*_json` column is
  `NOT NULL`, so pass `dbtypes.EmptyObject()`. `mr_findings` has no generated
  `Create`: every insert goes through `Insert`, which carries
  `ON CONFLICT (project_id, mr_iid, fingerprint) DO NOTHING`.
- **Status columns are plain `text` on purpose** — no CHECK. The closed sets live
  in `internal/dbtypes` (`ReviewStatus`, `DigestRunStatus`, `MessageStatus`),
  whose `Value()` (the method pgx calls when encoding a parameter) rejects
  anything else, and the four `store` wrappers (`CreateReview`, `CreateDigestRun`,
  `SetDigestRunStatus`, `CreateDigestMessage`) are the only parameterised way a
  status reaches a row. Do not re-add a CHECK — that splits one rule across two
  places. `TestSQLStatusLiterals`, `TestNoDirectStatusWrites` and
  `TestWrappersRejectInvalidStatus` keep both routes honest.
- **`numeric` is `decimal.Decimal`** (`shopspring/decimal`, mapped in
  `sql/pgxgen.yaml` for models and queries; nullable → `NullDecimal`). pgx needs
  no codec registered — it falls back to the type's `driver.Valuer`/`sql.Scanner`.
  **Compare with `.Equal`, never `==`**: a Decimal is a value+exponent pair, so
  `12.345678` read from `numeric(12,6)` is a different struct from the same
  amount built by `NewFromFloat`. The one column is `mr_reviews.cost_usd` and the
  single float64→Decimal conversion is in `service.persistReview`.
- **Pipeline presets** resolve in `runtime.go:pipelineFromConfig`: `cheap` |
  `standard` (default) | `deep` | `custom`. **Verifier trust levels:** the
  default `["go_build", "go_vet", "py_syntax"]` never execute repository code.
  `tsc`, `go_test`, `review.coverage.enabled` and `coverage.node.install` all run
  code from arbitrary repositories on a shared host and stay explicit opt-ins.
  Do not move any of them into a default.
- **Metrics.** Declared with `promauto` on the **default** registry in
  `internal/metrics`; other packages call the exported helpers and label
  constants. Two call-site rules: `gitlab_requests_total{endpoint}` must receive
  the **templated** path (wire it through `gitlab.Config.Observer`), and
  `SetTeamState` must be called every pass **including when all counts are zero**,
  or a stale gauge persists forever.
- **Lifecycle.** River starts on `context.WithoutCancel(ctx)` so mx's shutdown
  cannot hard-cancel in-flight jobs before `Stop` drains them; the jobs service is
  registered with `ShutdownTimeout = drain_timeout + 5s`; `launcher.WithService`
  is duck-typed; registration order is start order and shutdown is LIFO, so
  Postgres is registered first and stops last. The upstream helper really is
  spelled `launcher.ShutdownSiganl()`.
- **Logging** is `github.com/tkcrm/mx/logger` (zap). New code takes
  `logger.Logger`, never `*slog.Logger`.
- **Testing.** The engine, services, validator, line mapper, domain classifiers,
  markers and matcher are tested against fakes (`gitlab.FakeClient`,
  `gitlab.FakeGraphQL`, the `llm` fake) — no network, no `claude`. Store and job
  tests use `storetest` against a real database. Keep new logic pure enough to
  stay fake-testable, and always run with `-race`. **No review has ever run
  against a real GitLab instance**; every path is exercised through fakes, so
  treat first contact with a live instance as untested ground.
- **Comments explain *why*, not *what*.** This tree is unusually well commented
  at decision points, and most of those comments record a failure mode that was
  hit once. Match that density; do not strip them.

## Deployment

`Dockerfile` (multi-stage, `CGO_ENABLED=0`, Alpine runtime carrying the Claude
Code CLI, non-root, writable `/work`), `.dockerignore`, `docker-compose.yml` +
`.env.example` (the README's install path: PostgreSQL 17 and the service), and
`dev/docker/docker-compose.yml` (the dev database only — do not confuse the two
compose files).

- **No Kubernetes manifests and no vendored keys live here**, by explicit
  decision. Do not add them back: deployment topology belongs to whoever deploys,
  and nothing here should carry somebody else's public key in version control.
- **The Dockerfile has an `ENTRYPOINT` and deliberately no `CMD`.** The image is
  the whole CLI; which command runs is the caller's decision, and compose names
  `start` itself. A bare `docker run` prints help.
- **The agent is installed unpinned** (`apk add claude-code`), so rebuilding the
  image is how the reviewer moves forward. Accepted cost: two builds of one commit
  can ship different agents, and `claude --version` in the container is the only
  record. `DISABLE_AUTOUPDATER=1` still applies — the version may differ between
  builds, never inside one running container.
- **`USE_BUILTIN_RIPGREP=0` and `DISABLE_AUTOUPDATER=1` are set in the image and
  named in `claudeEnvNames`.** The allowlist is what carries them into the
  subprocess; removing a name there makes the image's `ENV` silently ineffective,
  and losing `USE_BUILTIN_RIPGREP` kills the `Grep` tool on musl.
- The service gets `AI_REVIEWER_CONFIG` as an environment variable rather than a
  `--config` flag, so `docker compose exec ai-reviewer ai-reviewer doctor` finds
  the config too instead of looking for `./config.yaml` in `/work`.
- Claude Code needs 4 GB+ RAM per concurrent process; `review.max_parallel` of
  them run in one replica.

## Docs

**The README is the landing page, not the manual** — what this is, why it exists,
how to install it, and links onward. Keep it that short; a change in behaviour
belongs in the matching guide: `docs/installation.md`, `docs/configuration.md`,
`docs/operations.md`, `docs/architecture.md`, `docs/security.md`,
`docs/development.md`. Links between them are relative and must resolve from
`docs/` (`../config.example.yaml`, not `config.example.yaml`).

`docs/competitive-analysis-*.md` and `docs/gopls-mcp-analysis.md` are analysis
history — evaluations, not specifications.
