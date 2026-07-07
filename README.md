<p align="center">
  <img src="screenshots/promo.webp" alt="ai-reviewer — local AI code review for GitLab merge requests" width="100%">
</p>

# ai-reviewer

Local, personal **AI code review for GitLab merge requests**. It finds the MRs
where _you_ are the reviewer, runs a deep AI review from your machine, and lets
you approve, edit, or reject each finding before anything is posted. Comments
are created as **GitLab draft notes** and published **only after you explicitly
confirm** — nothing is auto-posted, nothing auto-approves or merges.

No CI/CD changes required. It works entirely through the GitLab API v4 and a
local repo cache, driven from a **local web UI** (or the CLI).

---

## Highlights

- **Local-first.** A localhost-only web UI (random port + per-launch session
  token) and a SQLite state DB under `~/.ai-reviewer`. Your code and tokens stay
  on your machine.
- **You are the gate.** Findings are _proposed_ → you _approve/reject/edit_ →
  _create drafts_ → _publish_ (with a typed confirmation phrase). Auto-publish is
  hard-disabled by default.
- **Real position mapping.** Go — not the model — owns GitLab inline-comment
  positions (added→`new_line`, removed→`old_line`, context→both), with a
  fallback ladder to nearest changed line or an overview note.
- **Deduped & filtered.** Findings are fingerprinted so re-reviews don't spam;
  binary/vendored/generated files never reach the LLM; secrets are redacted from
  logs.
- **Pluggable LLM.** Ships with the **Claude CLI** provider (uses your existing
  Claude Code login); the `LLMClient` interface keeps other providers a drop-in.
- **Background watch mode.** An optional daemon syncs assigned MRs and queues a
  review when an MR's head sha changes (local report only, unless you opt in).

---

## Install

Requires **Go 1.26+**, **git**, and the **`claude` CLI** (logged in) for reviews.

```bash
go install github.com/sxwebdev/ai-reviewer/cmd/ai-reviewer@latest
```

## Quick start

```bash
ai-reviewer start     # opens the local web UI
```

On first run the web UI shows a setup screen: enter your GitLab host and a
personal access token (scope: `api`) — the token is verified live, your
username is auto-detected, and everything is saved to
`~/.ai-reviewer/config.yaml` (chmod 600). Directories and the SQLite DB are
created automatically. `ai-reviewer doctor` verifies the environment any time.

Then in the web UI: **Sync assigned MRs** → open an MR → **Run review** →
approve findings → **Create GitLab draft notes** → **Publish** (type the
confirmation phrase).

## CLI

| Command                    | Purpose                                                   |
| -------------------------- | --------------------------------------------------------- |
| `ai-reviewer doctor`       | Check GitLab auth/reachability, Claude, git, DB, FTS5, Go |
| `ai-reviewer start`        | Start the local web UI (+ background worker)              |
| `ai-reviewer daemon`       | Run the watch worker + scheduler headless                 |
| `ai-reviewer sync`         | One-shot sync of MRs assigned to you for review           |
| `ai-reviewer review <ref>` | One-shot review of one MR (local report only)             |

`<ref>` accepts a full MR URL, `group/sub/repo!123`, or `project-id:iid`.

Global flags: `--config <path>`, `--debug`. `start` adds `--host`, `--port`,
`--open`, `--daemon`. `daemon` adds `--interval`, `--auto-review`,
`--auto-draft`, `--auto-publish`, `--max-parallel`.

## GitLab token scopes

Create a **Personal Access Token** with the **`api`** scope and Developer+ role
on the projects you review (needed to create/publish draft notes; drafts are
per-user). Put it in `gitlab.token` in `~/.ai-reviewer/config.yaml` — the file
is created with `0600` permissions and stays on your machine. Alternatively,
leave `gitlab.token` empty and expose the token via the env var named by
`gitlab.token_env` (default `GITLAB_TOKEN`). Either way the token is masked in
all logs.

## Claude auth modes

Configured under `llm.claude`:

- `existing-login` (default) — uses your logged-in Claude Code session.
- `oauth-token` — reads `CLAUDE_CODE_OAUTH_TOKEN` (from `claude setup-token`).
- `api-key` — reads `ANTHROPIC_API_KEY` (future Anthropic API provider).

All `claude` flags are configurable (`llm.claude.bin`, `.model`,
`.permission_mode`, `.allowed_tools`, `.extra_args`) so the wrapper survives CLI
changes. Cost shows as "unavailable" on subscription/OAuth auth.

## Safety model

- **No auto-publish.** Publishing requires typing `PUBLISH <n> COMMENTS`; the
  server verifies `<n>` against the approved count. The daemon's auto-publish is
  a separate, off-by-default flag.
- **Draft-only by default.** Approved findings become GitLab _draft_ notes; you
  publish them as a distinct action.
- **Never** approves or merges an MR, resolves or deletes others' discussions.
- **No secrets in logs.** A redaction handler masks tokens, bearer headers, and
  emails from all logs and job output (private code only under `--debug`).
- **No binary/vendored/generated** files are sent to the LLM.
- **No pre-existing comments.** Findings must map to lines the MR changed.
- **Dedup.** Findings are fingerprinted (project+MR+file+category+title) so the
  same issue isn't repeated across head shas or against existing discussions.

## How it works

1. **Sync** — `GET /merge_requests?scope=reviews_for_me&state=opened` → SQLite.
2. **Context** — fetch MR metadata, diffs (rendered with explicit line numbers),
   commit messages, discussion content, pipeline status; include changed files'
   full content (budgeted), review memory, and — on re-review — the previous
   review's findings, their dispositions, and the interdiff since the last
   reviewed head.
3. **Agent mode (optional)** — mirror-clone the repo and check out a read-only
   worktree at the head sha (indexed into SQLite FTS5) so the LLM can inspect
   surrounding code; the prompt mandates an investigation protocol (read full
   files, grep callers of changed symbols) and lists FTS-suggested related
   files. Falls back to diff-only on any failure.
4. **Multi-pass LLM review** — configurable pipeline (`review.pipeline.mode`:
   `cheap`/`standard`/`deep`/`custom`): specialist passes (general,
   correctness, concurrency, security, cross-file contracts) run concurrently
   as strict-JSON reviews (`claude --output-format json --json-schema …`) and
   merge with cross-pass dedupe.
5. **Verify** — a skeptic pass re-reads the actual code and tries to REFUTE
   each finding (refuted → dropped, uncertain → demoted; blocking findings are
   never silently dropped), then deterministic verifiers run: `go build`
   refutes false "does not compile" claims, `go vet` corroborates
   correctness/concurrency findings, `go test` (opt-in) flags already-failing
   packages.
6. **Validate (Go owns this)** — schema, file-in-diff, line→position mapping,
   severity threshold, dedupe, secret scrub, max-comments cap, ranking.
7. **Quality reports** (alongside findings): a deterministic risk score
   computed from git history and diff stats (churn, bug-magnet files,
   sensitive paths, no-tests-touched, new dependencies); an acceptance-criteria
   audit comparing the MR's stated intent (description + commits) against the
   actual diff (done/partial/missing per criterion); and — opt-in — measured
   test coverage of the changed lines (runs the repo's own go test /
   vitest / jest, monorepo-aware, and lists added lines no test executes).
8. **Approve → draft → publish** — your decisions, then GitLab draft notes, then
   an explicitly-confirmed publish.

## Configuration

`~/.ai-reviewer/config.yaml` (created automatically on first setup). Any field can be overridden by
`AI_REVIEWER_<PATH>` env vars. Key sections: `app`, `gitlab`, `llm`, `review`
(including `review.pipeline` for pass/verification modes and `review.context`
for prompt-context budgets), `watch`, `index`, `storage`. See the generated
file for the full, commented set.

Cost/depth presets (`review.pipeline.mode`): `cheap` ≈ 1 LLM call, `standard`
(default) ≈ 3, `deep` ≈ 6 — pick `deep` when catching real bugs matters more
than tokens.

## Review memory & reviewer profile

- **Review memory** — persistent per-project or global rules injected into the
  prompt (e.g. "Handlers must pass context to the DB layer", "New endpoints
  require integration tests"). Rejecting a finding can save it as a
  _false-positive_ memory so similar findings are suppressed.
- **Reviewer profile** — tone, strictness, comment language, max comments,
  severity threshold, enabled categories, prefer-questions, allow-nits. Comment
  language is `review.preferred_comment_language`: `auto` (default, matches the
  MR description's language), `en`, or `ru`.
  The default is a careful senior engineer: high-signal, correctness/security/
  tests/architecture first, questions when uncertain.

## Development

```bash
make build   # build the binary
make test    # go test -race ./...
make lint    # golangci-lint
make serve   # build and run the web UI
```

Architecture: `internal/{config,state,security,gitlab,git,index,llm,review,
service,jobs,server,ui,app,cli}`. The `review` engine and services are pure and
tested with fake GitLab/LLM clients; state uses pure-Go `modernc.org/sqlite`
(no CGO) with FTS5.

## Known limitations

- Reviews need the `claude` CLI installed and authenticated.
- Agent mode clones repos over HTTPS with your token; large repos are mirrored
  once then fetched incrementally.
- Web UI polls (refresh to see a queued review complete); no live push yet.
- No TUI (Web UI is the only interface); tree-sitter/LSP/vector-search are future
  work. Draft-note position fields may vary by self-managed GitLab version — on a
  400 the finding degrades to an overview note rather than failing the review.

## License

MIT.
