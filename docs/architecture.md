# Architecture

How the pieces fit, what one review actually does, and where state lives.

---

## Overview

```text
              GitLab (source of truth about MRs)      Linear (In Review issues)
                         │ REST v4 + GraphQL              │ GraphQL, read-only
                         └──────────────────┬──────────────┘
                                       ▼
   ┌──────────────────────────────────────────────────────────────────┐
   │  ai-reviewer  (N replicas)                                        │
   │                                                                   │
   │   River periodic jobs        River workers                        │
   │   ├─ scan     (5m)  ──► scan_repo ──► review ──► publish_review   │
   │   ├─ digest   (per-team slots) ──► slack_send                     │
   │   └─ cleanup  (1h)                                                │
   │                                                                   │
   │   Slack Socket Mode (outbound WebSocket, optional)                │
   │   └─ /all, /my ──► slack_command                                  │
   │                                                                   │
   │   review engine: multi-pass Claude Code → skeptic → Go validator  │
   └──────────────────────────────────────────────────────────────────┘
          │                          │                         │
          ▼                          ▼                         ▼
    PostgreSQL                GitLab comments             Slack digest
 (operational state,          (+ hidden markers)
  River queue, leader)
```

Package layering, strictly downward
(`internal/cli → app → {jobs, service} → {store, gitlab, slack, match, git, llm, review, domain} → security`):

| Package                              | Responsibility                                                                                |
| ------------------------------------ | --------------------------------------------------------------------------------------------- |
| `internal/cli`                       | urfave/cli v3 command tree. Parses flags, **enqueues** work, does not do it                    |
| `internal/app`                       | composition root: config → clients → services → mx launcher; `doctor`                          |
| `internal/jobs`                      | River client, eight job kinds, periodic schedule, drain                                        |
| `internal/service`                   | one operation end to end: `ScanRepository`, `RunReview`, `PublishReview`, `BuildDigest`, `SendMessage`, `RunSlackCommand` |
| `internal/domain`                    | pure classifiers over an MR snapshot — no I/O, fully unit-tested                               |
| `internal/store` + `internal/models` | pgx pool, transactions, pgxgen-generated repositories                                          |
| `internal/gitlab`                    | REST v4 client, GraphQL review states, comment markers                                         |
| `internal/linear`                    | read-only GraphQL client for paginated `In Review` digest issues                                |
| `internal/slack` + `internal/match`  | Slack client, Block Kit builder, Socket Mode listener; GitLab user → Slack user matching       |
| `internal/review`                    | the review engine: passes, skeptic, **validator**, line mapper, verifiers, risk, completeness   |
| `internal/llm`                       | `Client` interface + the Claude Code CLI provider + deterministic auth env                     |
| `internal/git`                       | ephemeral mirror clone + detached worktree at the head SHA                                     |
| `internal/scheduler`                 | one daily schedule per team, from its `digest.slots`/`digest.timezone` (embeds tzdata)         |
| `internal/metrics`, `internal/security` | Prometheus collectors; secret redaction for logs and errors                                 |

### How one review runs

1. **Snapshot** — the MR is loaded once per pass: metadata, reviewers,
   approvals, discussions, diff versions, `head_pipeline`.
2. **Cheap classification** — draft, closed, unchanged head SHA, or already
   reviewed at this SHA all short-circuit before anything expensive happens.
3. **Context** — diffs rendered with explicit old/new line numbers, changed
   files' content (budgeted), commit messages, discussion text and — on a
   re-review — the previous findings plus the interdiff since the last reviewed
   head.
4. **Agent mode** — the repository is mirror-cloned and a read-only worktree is
   checked out at the head SHA under `review.workdir`; `claude` runs with that
   as its working directory and a tool allowlist that is read-only *and* scoped
   to the checkout: `Read(${worktree}/**)`, `Grep(${worktree}/**)`,
   `Glob(${worktree}/**)`, where `${worktree}` is substituted per review. No
   `Bash` rule of any kind — see [Security model](security.md) for why the
   `Bash(git …)` grants this list used to carry were a write primitive.
5. **Passes** — specialist LLM passes run concurrently and merge with cross-pass
   dedupe. `review.pipeline.mode` picks the set:

   | mode       | passes                                                        | verification    |
   | ---------- | ------------------------------------------------------------- | --------------- |
   | `cheap`    | general                                                        | off             |
   | `standard` | general, correctness                                           | skeptic (default) |
   | `deep`     | general, correctness, concurrency, security, contracts         | skeptic         |
   | `custom`   | whatever `review.pipeline.passes` lists                        | as configured   |

6. **Skeptic** — a verification pass that tries to *refute* each finding. It can
   only drop or demote, never add or rewrite; blocking findings are demoted,
   never silently dropped.
7. **Deterministic verifiers** — `go_build` drops false "does not compile"
   claims when the build is clean; `go_vet` and `py_syntax` annotate. `go_test`
   and `tsc` exist but are **off by default** because they execute the reviewed
   repository's code.
8. **Validation (Go owns this)** — file-in-diff check, line → GitLab position
   mapping (added→`new_line`, removed→`old_line`, context→both), the fallback
   ladder (exact line → nearest changed line → overview note), severity
   threshold, dedupe, secret scrubbing, body-length cap, max-comments cap,
   ranking. The model supplies a file, a line and a side — never a SHA, never a
   position.
9. **Persist and publish** — the review, its findings and the `publish_review`
   job are written in **one transaction**, so "reviewed but nobody will publish
   it" cannot happen. The publish job posts each finding as an inline
   discussion, then the summary note **last**.

Alongside the findings the engine produces three reports (they do not anchor to
changed lines, so they are never comments): a deterministic **risk score** from
git history and diff shape, an **acceptance-criteria audit** comparing the MR's
stated intent against the actual diff, and — opt-in — **changed-line coverage**.

---

## The team model

A team is a Slack channel plus a list of repositories:

```yaml
teams:
  - name: payments
    slack_channel: C012345678
    ai_review: { enabled: true }
    linear_team_ids: ["9cfb482a-81e3-4154-b5b9-2c805e70a02d"]
    repositories: [backend/payments, backend/billing, frontend/checkout]

  - name: platform
    slack_channel: C987654321
    ai_review: { enabled: false } # digest only, no AI review
    repositories: [platform/auth, platform/gateway]
```

Repositories are GitLab full paths or numeric project ids. Configuration is
validated fail-fast at startup: team names must be unique (case-insensitively),
every team needs at least one repository and a Slack channel, and **one
repository may not belong to two teams** — the error names both.

`linear_team_ids` is optional. Each UUID may belong to only one application
team. Linear provides an `In Review` aggregate and two gates on linked GitLab MRs:
title identifier first, source branch fallback.

- **Readiness.** A card that has not reached `In Review` — graded against that
  Linear team's own column order, not a list of names — asks nobody to review and
  becomes an author action instead. It outranks approvals and `REQUESTED_CHANGES`.
- **Completion.** At `In Review` or later, one readable approval stops notifying
  remaining reviewers unless `REQUESTED_CHANGES` exists; an approved card still in
  `In Review` becomes an author action.

Both fail open into the ordinary GitLab classification, and every suppression on
the readiness side produces an author row, so no merge request can leave the
digest because of board state.

`ai_review.enabled: false` turns off automated review for that team while
keeping it in the digest. Digest scheduling is per team, so one team's failure
does not affect another's.

---

## The state model

**GitLab and Linear remain the sources of truth for their work items. PostgreSQL
holds the service's operational state.** The service never mirrors their data; it
records what *it* did.

There is a deliberate second record: every note the service publishes carries a
machine-readable HTML-comment marker, invisible in GitLab's rendering.

- each finding comment carries its **fingerprint**
  (`project + MR + file + category + title`, head-SHA-independent);
- the summary note carries a **review marker** with the head SHA, the timestamp,
  the finding count and the pipeline preset.

Those markers cost nothing — they are written into notes published anyway and
read from discussions loaded anyway — and they make the database *recoverable*:
if it is lost or restored from a backup, publication reads the discussions,
adopts the comments already present instead of reposting them, and moves on.

Publication is idempotent by construction: only findings with `note_id IS NULL`
are posted, each note id is recorded immediately after its POST rather than
batched, and **the summary marker is written last** — so `status='succeeded'`
can never be a false success. A review that is persisted but not yet published
is picked up again by the next scan after 15 minutes.

Slack delivery closes the same window with state instead of an idempotency key
(`chat.postMessage` has none): the row is claimed, the status it had *before*
the claim decides the branch, and a part left in `sending` by a crashed worker
is **resent deliberately** — a duplicate in a channel is noise, a lost digest is
a team that never finds out something was waiting. Those resends are counted by
`slack_resend_uncertain_total`.

---

## User matching

Digest mentions come from matching GitLab users to Slack accounts, in this
order:

1. **`slack.user_map` override** — `gitlab_username → Slack id | @handle | email`.
   A **user id** is answered without consulting the directory, so it keeps working
   while `users.list` is down — which is the case the override exists for. A
   handle or an email costs one directory read and is the form you can look up by
   hand. An override that does not resolve stops there and is logged: it is a
   configuration mistake, and falling through to the name probes below would hide
   it behind a plausible-looking mention.
2. **Email** — the GitLab user's email, normalised, against the Slack email
   index.
3. **Username / display name** — GitLab `username` against Slack
   `name`/`display_name`/`real_name`, then GitLab `name` against
   `real_name`/`display_name`. Case-insensitive, whitespace-collapsed, no
   aggressive fuzzy matching.
4. **Ambiguous** (more than one equally good candidate) — **no** pick is made. A
   wrong mention is worse than a missing one; the digest names the person in
   plain text and `slack_user_match_total{result="ambiguous"}` records it.
5. **Not found** — the digest still lists them as `John Smith (@john)`, without
   a mention.

The first probe that yields any candidate decides; there is no fall-through from
an ambiguous result to a later probe. Deleted, bot and app accounts are not
indexed at all — a deleted account holding a display name would otherwise shadow
the live human who inherited it.

> **Known limitation.** GitLab exposes another user's `email` only to an **admin**
> token; an ordinary service-account token sees just `public_email`, which is
> usually empty. Step 2 therefore often cannot fire, which is exactly why
> `slack.user_map` exists. Unmatched users are observable through
> `slack_user_match_total{result}` and the log — check it after the first digest
> and add overrides for whoever did not resolve.

---

## Limitations

Documented honestly, because each of these is a decision rather than a bug:

- **GraphQL is optional and may be unavailable.**
  `mergeRequestInteraction.reviewState` does not exist on older self-managed
  GitLab. The service falls back to a REST heuristic automatically — less
  precise about who has already reviewed, but functional. `doctor` reports which
  path is active.
- **Pipeline visibility depends on the token's role.** Below Reporter,
  `head_pipeline` is simply omitted and the "pipeline failed" line silently never
  appears.
- **"waiting 18h" is measured from the last push, not from assignment.** GitLab
  does not expose when a reviewer was assigned (there are no reviewer resource
  events), so the waiting time is computed from the `created_at` of the newest
  diff version. Semantically it reads as "how long this reviewer has not reacted
  to the MR's current state", which is usually what you want — but it is not
  assignment age, and a push resets it.
- **`go_test`, `tsc` and coverage execute repository code**, which on a shared
  host means running arbitrary code from every repository you watch. They are
  off by default and should stay off unless you control the repositories.
- **Cost.** Every new head SHA triggers a full pipeline (two passes plus the
  skeptic in `standard`). The controls are: the reviewed-SHA record in the
  database, `review.max_parallel`, one attempt per review job, cheap
  classification before any expensive call, the failure backoff ladder, and the
  `ai_review_cost_usd_total` / `ai_reviews_*` metrics. The cost metric is only
  as good as the `total_cost_usd` the `claude` CLI reports for a call — treat a
  flat zero as "not reported", not as "free".
- **Rate limits.** Self-managed GitLab limits requests per user and Slack's
  `users.list` is Tier 2. Mitigations: one directory load per run behind a
  singleflight, one snapshot per MR per pass, bounded concurrency, `Retry-After`
  honoured (bounded by `gitlab.max_retry_after`) and exponential backoff.
- **PostgreSQL is now an operational dependency.** If it is unavailable the
  service stops working: readiness fails and no jobs are claimed. That is the
  deliberate price of running several replicas correctly.

---

[← back to the README](../README.md)
