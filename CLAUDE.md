# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Local, single-user AI code review for **GitLab merge requests**. It finds MRs where you are the reviewer, runs an LLM review from your machine, and lets you approve/edit/reject each finding before anything is posted. Comments become GitLab **draft notes** and are published **only after an explicitly-typed confirmation** — nothing auto-posts, auto-approves, or merges. Works entirely through the GitLab API v4 plus a local repo cache; no CI/CD changes.

## Commands

```bash
make build          # build to bin/ai-reviewer (embeds version via -ldflags)
make test           # go test -race ./...
make lint           # golangci-lint run ./...
make fmt            # gofmt -s -w .
make serve          # build + run the local web UI

go test -race ./internal/review/...           # one package
go test -race -run TestValidate ./internal/review   # one test
```

Runtime commands: `ai-reviewer {init,doctor,serve,daemon,sync,review <ref>}`. Run `doctor` after config changes — it verifies GitLab auth, the `claude` CLI, git, the DB, and FTS5. `<ref>` accepts a full MR URL, `group/sub/repo!123`, or `project-id:iid` (see `internal/gitlab/ref.go`).

Requires Go 1.26+, `git`, and an authenticated `claude` CLI for reviews.

## Architecture

The flow is **CLI → app (composition root) → service layer → review engine + validator → state (SQLite)**. Layers below the service are pure and I/O-free, which is what makes them testable with fakes.

- **`internal/cli`** — thin urfave/cli v3 launcher. Parses flags, calls `bootstrap` to load config + build the `App`, dispatches to lifecycle entrypoints. No business logic here.
- **`internal/app`** — composition root. `App` wires config/logging/state/clients/services. Lifecycle methods (`Serve`, `RunDaemon`, `SyncOnce`, `ReviewOnce`) are the CLI's entrypoints. `Services()` builds the `service.Bundle`; when GitLab isn't configured it substitutes `gitlab.Unconfigured()` so the web UI still starts and surfaces a clear error on use.
- **`internal/service`** — orchestrates one operation end to end (fetch MR context from GitLab → run engine → persist). `Bundle` groups `Sync/Review/Finding/Publish`. Read paths hit `DB` directly; write/action paths go through services.
- **`internal/review`** — the pure engine + **validator**. `Engine.Review` runs a 6-stage pipeline (`pipeline.go`): fan-out of specialist LLM passes (`passes.go`/`pass_prompts.go`: general, correctness, concurrency, security, contracts — bounded concurrency) → merge with cross-pass dedupe (file+normalized-title) → deterministic `Validator` at a relaxed cap (2× MaxComments) → skeptic verification (`skeptic.go`: agent-mode LLM call that tries to REFUTE each finding; can only drop/demote, never add or rewrite; blocking findings are demoted, never dropped) → deterministic `Verifier` plugins (`verifier*.go`: `go_build` drops false compile claims on a clean build, `go_vet`/`go_test` annotate-only) → final rank + cap. Zero-value `PipelineConfig` = single general pass, verify off. Prompt context (`context.go`, `prompts.go`) adds budgeted full-file content, commits, discussion notes, prior-review continuity (findings + rejection reasons + interdiff), and FTS-suggested related files; diffs render with explicit old/new line numbers (`diff_render.go`). The engine does no I/O beyond LLM calls and verifier subprocesses.
- **`internal/llm`** — `Client` interface; `claude_cli.go` shells out to the `claude` CLI (`--output-format json --json-schema`). `fake.go` backs engine tests.
- **`internal/gitlab`** — API v4 client behind the `API` interface, with `fake.go` and `Unconfigured()` implementations.
- **`internal/state`** — SQLite via pure-Go `modernc.org/sqlite` (no CGO) + FTS5. Migrations in `migrations.go`; repositories split by entity.
- **`internal/git` + `internal/index`** — agent-mode support: mirror-clone the repo, add a read-only worktree at the head SHA, index it into FTS5.
- **`internal/jobs`** — durable background worker: the SQLite `jobs` table is the source of truth, a bounded goroutine pool claims jobs, a scheduler enqueues a review when an MR's head SHA changes.
- **`internal/server` + `internal/ui`** — localhost-only web UI (HTMX/Alpine, embedded templates), backed by the same `service.Bundle`.

## Invariants — do not weaken these

These are the point of the tool. Changes that erode them are almost always wrong.

- **Go owns validation and positions, not the model.** `internal/review/validator.go` + `line_mapper.go` own file-in-diff checks, line→GitLab-position mapping (added→`new_line`, removed→`old_line`, context→both), the fallback ladder (exact line → nearest changed line → overview note), severity threshold, dedupe, secret scrubbing, body-length cap, and the max-comments cap. The LLM only supplies a file + line + side; it never provides SHAs or the final position.
- **No auto-publish.** Publishing requires the user to type `PUBLISH <n> COMMENTS`; the server verifies `<n>` against the approved count. Findings flow: proposed → approved/rejected/edited → draft notes → explicitly-confirmed publish. The daemon's `--auto-publish` is a separate, off-by-default flag.
- **Never** approve/merge an MR, or resolve/delete others' discussions.
- **Findings only on changed lines.** A finding whose file isn't in the MR diff is dropped — never comment on pre-existing code.
- **Dedup by fingerprint** (project+MR+file+category+title) so re-reviews across head SHAs don't spam.
- **Binary / vendored / generated files never reach the LLM** (`parseDiffs` + `isVendored` in `internal/service/review.go`).
- **No secrets in logs.** `internal/security` registers resolved secrets and a redaction handler masks tokens/bearer headers/emails from all logs and job output.

## Conventions

- **Pipeline presets**: `review.pipeline.mode` = `cheap` (1 call, verify off) | `standard` (general+correctness+skeptic, default) | `deep` (5 passes + skeptic) | `custom` (`passes:` list); resolved in `internal/app/services.go:pipelineFromConfig`. `review.context.*` bounds prompt enrichment budgets. Per-pass provenance and skeptic outcome persist in `findings.pass`/`findings.verification`; per-pass cost/duration in `reviews.pipeline_json`.
- **Quality reports** (reports, never findings — they don't anchor to changed lines): deterministic risk score (`review.ComputeRisk` + `git.Cache.RecentHistory` batched log; `reviews.risk_json`), acceptance-criteria audit (`review/completeness.go`, concurrent LLM side-call, `review.pipeline.completeness: on|off|auto`; `reviews.completeness_json`), and opt-in changed-line coverage (`internal/coverage`: providers go/node behind an injectable Runner, LCOV + Go coverprofile parsers, pure `Intersect`; `reviews.coverage_json`). `internal/toolchain` (stdlib-only) owns monorepo nearest-root discovery and test-path/glob classification — `review` → `coverage` → `toolchain`, never the reverse.
- **Verifier trust levels**: default `["go_build", "go_vet", "py_syntax"]` never execute repository code (py_syntax is a pure `ast.parse`); `tsc` (runs node_modules tsc) and `go_test` execute repo code and stay opt-in. Same rule for `review.coverage.enabled` (runs repo tests) and `coverage.node.install` (runs package-manager lifecycle scripts) — both explicit opt-ins.
- **Config**: `~/.ai-reviewer/config.yaml` (created by `init`, `0600`). Any field overridable by `AI_REVIEWER_<PATH>` env vars. The GitLab token is stored in `gitlab.token` in the config file by design (not env-only) — see the `gitlab-token-in-config` memory. `gitlab.token_env` (default `GITLAB_TOKEN`) is the fallback when `gitlab.token` is empty.
- **Testing**: engine, services, validator, and line-mapper are tested against `fake.go` GitLab/LLM clients — no network, no `claude` CLI. Keep new logic in these layers pure so it stays fake-testable. Always run with `-race`.
- **Claude CLI wrapper**: every `claude` flag is config-driven (`llm.claude.{bin,model,permission_mode,allowed_tools,extra_args}`) so the wrapper survives CLI changes — don't hardcode flags.
