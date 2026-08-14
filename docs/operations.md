# Operations

Running the service day to day: the commands, the dry-run switches, the
schedules, what to watch, and what the common failures look like.

---

## Commands

```bash
ai-reviewer start                              # the service: River workers, periodic jobs, ops server
ai-reviewer scan   [--team <name>]             # enqueue a scan pass
ai-reviewer digest [--team <name>] [--force]   # enqueue a digest run for the current slot
ai-reviewer review <ref> [--publish] [--wait]  # enqueue a review of one MR
ai-reviewer review <ref> --local [--publish]   # run it in this process (debugging)
ai-reviewer doctor [--local]                   # diagnose config and every dependency
ai-reviewer migrations up|down|create          # database schema
```

Global flags: `--config <path>` (repeatable, applied in order; also
`$AI_REVIEWER_CONFIG`) and `--debug`.

`<ref>` accepts a full merge-request URL, `group/subgroup/repo!123`, or
`project-id:iid`.

**The CLI enqueues; a running `start` executes.** That keeps exactly one
implementation of every operation, and makes a manual run subject to the same
uniqueness, retries and metrics as a scheduled one.

- `--wait` polls the queued job to a terminal state and prints the result.
- `--publish` lives in the *job's arguments*, not in process config, and is
  deliberately outside the job's uniqueness key. If your request collapses into
  a review that is already running without publication, the CLI says so instead
  of exiting quietly successful.
- `--local` runs the worker's code in the CLI process. It is not covered by
  River's unique jobs, so it first takes a Postgres advisory lock on
  `review:<project>:<iid>:<sha>`; if the lock is held it exits telling you the
  SHA is already being reviewed. **The queued worker takes the same lock**,
  which is what makes that exclusion real in both directions — a worker that
  finds it held snoozes for a minute instead of failing, so it does not burn its
  single attempt. A local run writes the same rows and enqueues publication
  exactly like the job would.
- **A skip is a success, and the CLI says which one.** `up_to_date`, `draft`,
  `disabled`, `not_open` and `head_moved` each print what happened and what to
  do about it. `head_moved` is the one that reads like a failure and is not: the
  MR was pushed to between the moment the review was queued (or the ref
  resolved) and the moment the worker loaded the snapshot, so nothing was
  reviewed and nothing was recorded — the review is bound to the SHA it was
  queued for, because that SHA is the queue's uniqueness key. The next scan pass
  queues the head that is live now; re-running the command does the same.
- `digest` for a slot that already ran is refused with a readable message;
  `--force` inserts a new *attempt*, which produces a fresh `digest_runs` row
  and an honest journal of manual repeats.

`ai-reviewer doctor` checks, in order: configuration, GitLab TLS, `git`, the
`claude` binary and `claude auth status --json` **under the configured auth
mode**, the `Europe/Moscow` timezone, PostgreSQL connectivity, pending
migrations, River's tables, GitLab authentication (`GET /user`), GraphQL
availability, every configured repository, `head_pipeline` visibility, Slack
`auth.test`, bot membership of each channel, and whether `review.workdir` is
writable. Secret values are never printed. `--local` skips the network probes. A
failed check exits non-zero.

The TLS line is a `WARN`, not a failure: `gitlab.insecure_skip_verify` is
honoured if you set it, and this is the only place that says so out loud —
certificates go unverified on every call, including the ones carrying the PAT.
Prefer `gitlab.ca_cert_path` for a private CA (it replaces the root pool rather
than appending to it).

---

## Dry-run modes

Both are on by default, so a fresh deployment observes and records without
writing anything to GitLab or Slack.

**`service.ai_review_publish_enabled: false`** — merge requests are scanned,
reviewed, validated and persisted; the review row is stored with
`status='dry_run'` and no `publish_review` job is created. Storing it is what
stops the scanner from re-reviewing the same head SHA every five minutes, and
what lets you inspect the findings the service *would* have posted.

`ai-reviewer review <ref> --publish` overrides the switch for one run — including
for a SHA that was **already** reviewed as a dry run. That case used to be a dead
end: the stored row makes the MR `up_to_date`, so the review was skipped, while
`publish_review` is a deliberate no-op for a dry run and the stale-publication
sweep only looks at `reviewed` rows — the findings were unreachable until
somebody pushed. Now the row is promoted to `reviewed` and its publication
enqueued in one transaction, so no tokens are spent re-reviewing and the command
reports `reviewed: N finding(s)` rather than `skipped: up_to_date`.

**`service.slack_send_enabled: false`** — repositories are scanned, MRs
classified, users matched and the Block Kit payload built and stored, with
`digest_messages.status='dry_run'`, the payload logged as a preview and no
`slack_send` jobs enqueued. `digest_runs.status` is `dry_run`. Everything except
the network call actually happens, so what you inspect is the real output.

---

## Schedules

| Schedule                   | What it does                                                     |
| -------------------------- | ---------------------------------------------------------------- |
| `review.scan_interval` (5m) | enqueues `scan`, which fans out one `scan_repo` per repository   |
| **09:00 and 16:30 Europe/Moscow** | enqueues one `digest` per team                            |
| `jobs.cleanup_interval` (1h) | sweeps orphaned worktrees and stale mirrors under `review.workdir` |

Periodic jobs are inserted by the River **leader** only, so they fire once
across all replicas. `scan` and `cleanup` also run immediately on start (a fresh
replica should not idle for a whole interval, and a SIGKILL is exactly what
leaves worktrees behind); `digest` does not, so a restart at 11:00 cannot fire
the 09:00 slot.

**The digest times are Europe/Moscow and are not configurable.** The zone is
resolved from tzdata embedded in the binary (`internal/scheduler` imports
`time/tzdata`), so the schedule does not depend on the container's local time or
on the image shipping a zone database. The digest's `run_date` and slot are
resolved in that zone too — at 23:30 UTC the Moscow calendar day is already
tomorrow.

Per-SHA failures back off on a fixed ladder — **15m, 1h, 6h, then stop** — until
a new head SHA resets the count. A deterministically-broken MR would otherwise
cost 288 full LLM runs a day. Review jobs get **one attempt** (a failed review
has already paid for its tokens); publication and Slack delivery get ten,
because they are cheap network retries.

The ladder counts the merge request's failures and only those. The attempt row
is written *before* the expensive work, so a review killed by an OOM or a
SIGKILL still counts even though River never re-runs it — that is exactly the
pathological case (huge diff → huge prompt → OOM) the ladder exists for. In the
other direction, a review cancelled by **our own shutdown** discards its row and
records `ai_reviews_failed_total{reason="canceled"}` instead, so a rolling
deploy landing on a long review is not a strike. A job *timeout* does count: it
arrives as a deadline rather than a cancellation, and a diff too large to finish
is the MR's problem.

---

## Observability

The mx ops server exposes, on `ops.*.port` (10000 by default):

| Path           | Purpose                                              |
| -------------- | ---------------------------------------------------- |
| `/livez`       | liveness                                             |
| `/readyz`      | readiness — fails while Postgres is unreachable      |
| `/metrics`     | Prometheus                                           |
| `/debug/pprof` | profiler — **off by default**, see below             |

The profiler is the one endpoint that is not served out of the box: this port
carries no authentication of its own, so heap and goroutine dumps would be
readable by anything that can reach it. Turn
it on deliberately, once the port is restricted:

```yaml
ops:
  profiler: { enabled: true, port: "10000" }
```

or `AI_REVIEWER_OPS_PROFILER_ENABLED=true`. Without it `/debug/pprof` answers
404 while `/livez`, `/readyz` and `/metrics` work normally.

Metrics worth alerting on:

| Metric                                            | Meaning                                        |
| ------------------------------------------------- | ---------------------------------------------- |
| `ai_reviews_total{result}` / `ai_reviews_failed_total{reason}` | review outcomes. `reason` is a closed set: `gitlab`, `diff`, `llm`, `persist`, `canceled` |
| `ai_reviews_skipped_total{reason}`                | why an MR was not reviewed — `up_to_date`, `draft`, `disabled`, `not_open`, `no_head_sha`, `head_moved` |
| `ai_review_duration_seconds`, `ai_review_cost_usd_total` | cost and latency of reviews             |
| `ai_reviewer_scans_total{result}`, `ai_reviewer_scan_duration_seconds` | scan health              |
| `gitlab_requests_total{endpoint,method,status}`, `gitlab_request_errors_total` | API health (templated paths, low cardinality) |
| `merge_requests_scanned_total{team}`               | counter: open MRs inspected                    |
| `merge_requests_waiting_human_review_total{team}`, `merge_requests_with_unresolved_threads_total{team}`, `merge_requests_with_conflicts_total{team}`, `merge_requests_with_failed_pipeline_total{team}` | per-team **gauges**, rewritten every pass — this is the digest's state as a dashboard |
| `slack_digest_runs_total{team,result}`, `slack_messages_sent_total`, `slack_send_errors_total`, `slack_resend_uncertain_total` | digest delivery |
| `slack_user_match_total{result}`                  | matching quality — watch `ambiguous`/`not_found` |
| `river_jobs_total{kind,state}`, `river_job_duration_seconds`, `river_job_retries_total` | queue health |

**Exclude `reason="canceled"` from any "this merge request is poisoned" panel or
alert.** It means our own shutdown cancelled the review — a rolling deploy
landing on a long review is the ordinary case — and not that the MR failed. It
is a separate label value precisely so it can be filtered out, and the row it
would otherwise have written is discarded rather than counted against the
per-SHA backoff ladder, so four deploys can no longer blacklist a healthy SHA:

```promql
sum by (team, reason) (increase(ai_reviews_failed_total{reason!="canceled"}[1h]))
```

Watching `canceled` on its own is still useful — a rise means reviews are being
killed mid-flight, i.e. `jobs.drain_timeout` is too short for how long reviews
actually take.

---

## Troubleshooting

**Start with `ai-reviewer doctor`.** It covers most of what follows, and prints
no secret values, so its output is safe to paste into a ticket.

| Symptom                                            | Likely cause                                                                                             |
| -------------------------------------------------- | -------------------------------------------------------------------------------------------------------- |
| Nothing happens at all after `start` runs           | Both dry-run switches are off by default. Check `service.*_enabled`, and look for persisted `dry_run` rows |
| `config` check fails listing several problems       | Fail-fast validation. It reports every problem at once — fix them together                                |
| `river schema` check fails                          | `ai-reviewer migrations up` was never run against this database                                          |
| `migrations` check reports pending                  | A replica started with `migrate_on_start: false`; run `ai-reviewer migrations up`, or turn it back on     |
| Reviews never start, `readyz` red                   | Postgres unreachable. The service is designed to stop rather than proceed blind                          |
| "pipeline failed" never appears in the digest       | The token cannot see pipelines — the service account needs at least Reporter (§ GitLab permissions)      |
| Digest posts nothing, token looks fine              | The bot is not a member of the channel — `doctor` checks this per channel                                |
| Everyone in the digest appears without a mention    | `users:read.email` scope missing, or GitLab is not exposing emails; add `slack.user_map` overrides       |
| One person appears as plain text while others ping  | Ambiguous match. Add a `slack.user_map` entry for them                                                   |
| `claude auth` check fails                           | The mode and the credential disagree. `oauth-token` needs `AI_REVIEWER_CLAUDE_CODE_OAUTH_TOKEN`; `existing-login` will not work in a container |
| Reviewer states look coarse                         | GraphQL is disabled or unsupported; the REST heuristic is in use. `doctor` says which                    |
| A review job runs but publishes nothing             | It ran as a dry run. Re-run with `ai-reviewer review <ref> --publish --wait`                              |
| `scan --team X` reports it was folded in            | A full scan was already in flight; scan uniqueness is by kind. Wait for the next pass                    |
| A finding lost its line anchor                      | GitLab rejected the stored position; the finding is posted as an unpositioned discussion rather than lost |
| The same MR keeps failing                           | The 15m/1h/6h backoff is in effect; after the fourth failure it stops until a new head SHA               |
| Worktrees accumulate under `review.workdir`         | The cleanup job runs hourly and on start; check the `cleanup` job in `river_job`                          |

---

[← back to the README](../README.md)
