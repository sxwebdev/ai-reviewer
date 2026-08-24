# Operations

Running the service day to day: the commands, the dry-run switches, the
schedules, what to watch, and what the common failures look like.

---

## Commands

```bash
ai-reviewer start                              # the service: River workers, periodic jobs, ops server
ai-reviewer scan   [--team <name>]             # enqueue a scan pass
ai-reviewer digest [--team <name>] [--force]   # enqueue a digest run for the current slot
ai-reviewer digest --dry-run [--team <name>]   # build one here and print it: no rows, no Slack
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
uniqueness, retries and metrics as a scheduled one. `review --local` and
`digest --dry-run` are the two exceptions, and both run the service's own code
rather than a copy of it.

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
- **`digest --dry-run`** (alias `--preview`) assembles the digest in this
  process and prints it. It reads GitLab, Linear and the Slack directory exactly
  as the scheduled build does — it *is* the same assembly, with the persistence
  and the delivery removed rather than reimplemented — and then writes nothing:
  no `digest_runs` row, no `digest_messages`, no `slack_send` job, no
  `chat.postMessage`. It consumes no slot, so it never collides with the
  scheduled run, and unlike `slack_send_enabled: false` it needs no config
  change and no deploy. Mentions are printed as names, because a terminal draws
  `<@U024BE7LH>` as an id; `--json` prints the untouched `chat.postMessage`
  payloads instead, for checking what Slack actually receives.

  ```bash
  ai-reviewer digest --dry-run --team blockchain-api
  ```

`ai-reviewer doctor` checks, in order: configuration, GitLab TLS, `git`, the
`claude` binary and `claude auth status --json` **under the configured auth
mode**, PostgreSQL connectivity, pending
migrations, River's tables, GitLab authentication (`GET /user`), GraphQL
availability, every configured repository, `head_pipeline` visibility,
`/approvals` visibility, each team's
resolved digest schedule (slots, timezone and skipped days, printed in full —
with per-team schedules there is no other way to answer "when does ours arrive",
and without the skips no way to answer "why did nothing arrive today"), Linear
authentication/team UUIDs/`In Review` states, each team's review-gate column
split, Slack
`auth.test`, bot membership of each channel, the Slack **directory**
(`users.list` — it names the missing scope, and warns when no member exposes an
email), every `slack.user_map` entry resolved against that directory, the in-chat
commands (the app-level token proved against `apps.connections.open`, plus the
two command names this deployment answers to), and whether
`review.workdir` is writable. Secret values are never printed. `--local` skips
the network probes. A failed check exits non-zero.

The directory check exists because its absence was invisible: a token without
`users:read` passes `auth.test` and every channel check, then fails at 09:00 and
the digest names everybody without mentioning them.

The TLS line is a `WARN`, not a failure: `gitlab.insecure_skip_verify` is
honoured if you set it, and this is the only place that says so out loud —
certificates go unverified on every call, including the ones carrying the PAT.
Prefer `gitlab.ca_cert_path` for a private CA (it replaces the root pool rather
than appending to it).

---

## Dry-run modes

Both are on by default, so a fresh deployment observes and records without
writing anything to GitLab or Slack.

**Neither of them is the switch that spends money.** Reviewing is per team —
`teams[].ai_review.enabled` — and it is the only thing that decides whether the
LLM runs at all. A dry run costs exactly what a published one costs, a few dollars
per merge request per head SHA, and shows nothing in GitLab in return, so the
combination "reviewing on, publishing off" is a deliberate, temporary state. The
service says so at startup:

```text
info  effective mode  {"ai_review_teams": ["payments"], "digest_only_teams": [],
                       "publish_findings": false, "send_to_slack": true,
                       "agent_mode": true, "workdir": "./data", "model": "sonnet"}
warn  reviews will run but nothing will be published to GitLab  {"teams": ["payments"], ...}
```

That is the first place to look when the service is reviewing and you did not
expect it to. `config.example.yaml` ships every team with `ai_review.enabled:
false` for the same reason — pointed at a busy project, the very first scan pass
would otherwise review every open merge request at once.

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

To read one digest without changing the deployment at all, use
`ai-reviewer digest --dry-run` instead: the same build, printed to your terminal,
recording nothing.

---

## In-chat commands

Two slash commands answer between the scheduled slots, when the app-level token
is configured (see
[installation.md](installation.md#in-chat-commands-optional)):

| Command | Answer                                                   | Who sees it        |
| ------- | -------------------------------------------------------- | ------------------ |
| `/all`  | the whole team's digest, exactly as the schedule posts it | the whole channel  |
| `/my`   | only the caller's rows                                    | only the caller    |

- **The channel picks the team.** A team owns exactly one channel, so a command
  typed in it can only mean that team. In a DM or any other channel the command
  is refused by name — on a one-team deployment it falls back to that team,
  because there is nothing else it could mean.
- **`/my` is a filter over the same digest**, not a second classification: the
  digest already carries the Slack id of everybody it names. That is what stops
  the command and the schedule from ever answering differently. Its empty answer
  names both readings — nothing waiting, *or* a Slack account not matched to a
  GitLab one — because the service genuinely cannot tell them apart, and "you are
  clear" would be the wrong guess to make silently.
- **Nothing is recorded.** No `digest_runs` row, no `digest_messages`, no slot
  consumed, so a command can never collide with the scheduled run. Same contract
  as `digest --dry-run`.
- **A repeat folds into the answer already coming.** Uniqueness covers team,
  scope, caller and channel, so a double press in the same conversation does not
  start a second GitLab pass; the second acknowledgement says so. The same
  command in a *different* conversation is a different question and is answered
  separately — the answer is delivered to the response URL of the invocation that
  won the key, so folding two conversations would acknowledge one and answer the
  other.
- **Slack's own budget is five messages per command.** A digest longer than that
  is cut with a visible line saying how many parts are missing — the scheduled
  digest carries the whole list.
- **A Slack outage does not stop the service.** If the connection cannot be
  opened at startup the process still comes up, logs
  `slack socket: first connection failed, retrying in the background` and dials
  again with backoff; commands start working when Slack does. Only a token this
  app can never use (`invalid_auth`, `not_allowed_token_type`, a 401/403 — what
  `doctor` probes) fails startup, because no retry fixes it and the log line is
  the answer. Reviews, scans and digests are never held hostage to the socket.
- `slack_commands_total{team,scope,result}` counts them.

---

## What the digest looks like

One block per person, answering "what does this person owe": the reviews they
have not delivered, then their own merge requests that need work.

```text
📋 MR Digest — blockchain-api

@dkhristoliubov · to review 11 · your MRs 3
🔴 review !1392 · waiting 10d — CHAIN-206: raise FIREBLOCKS_PROXY_TIMEOUT above…
🟡 review !1358 · waiting 5d — CHAIN-170 index EVM ERC20 deposits via Transfer…
🟡 review !1327 · waiting 4d — CHAIN-104 partial index for unsynced blocks
   +8 more to review: !1363 !1365 !1369 !1356 !1390 !1385 !1403 !1404
🛠 your MR !1378 · 💬 resolve 6 threads — CHAIN-122 Consolidation deposit detection
🛠 your MR !1366 · 💬 resolve 3 threads · ⚠️ fix merge conflicts — CHAIN-184 Scheduler…
🛠 your MR !1375 · 🔁 address changes requested by @apyshinskii — CHAIN-203 Delegator…
```

Reading it:

- **Every row names the action it asks for**, and every flag is an imperative:
  `review`, `your MR`, `resolve N threads`, `fix merge conflicts`,
  `fix the failed pipeline`, `move CHAIN-N to In Review`. The icons are there to
  make the list scannable once you know them — they are not what carries the
  meaning, because a digest whose rows have to be decoded is one nobody reads
  twice.
- **People are ordered by how much they owe**, most first. The biggest queue is
  the one worth looking at, and alphabetical order buried it.
- **Reviews:** the three oldest get a full row; the rest are the
  `+N more to review:` line. Every merge request is linked — nothing is dropped,
  only shortened, and the tail names the action once rather than on every entry.
- **Age markers** are 🔴 from a week, 🟡 from two days, ▫️ below that. They
  classify; they do not filter, and the row spells the wait out as
  `waiting 10d` beside them.
- **Own merge requests** are the `🛠 your MR` rows, with the flags in a fixed
  order: changes requested → threads → conflicts → pipeline → advance Linear
  card → move Linear card to In Review.
- **The project name** sits in the title when the digest covers one repository,
  and moves into each row when it covers several — it is what tells two `!1404`s
  apart.
- **Titles** are cut to 55 characters at a word boundary. The ticket key comes
  first in practice, so it survives the cut.
- **Linear-aware readiness:** a linked MR whose card has not reached `In Review`
  asks **nobody** to review it. Its author gets
  `move CHAIN-N to In Review (now In Progress)` instead — the column is named
  because the difference between `In Progress` and `Canceled` is the difference
  between moving the card and closing the merge request. This outranks approvals
  and `REQUESTED_CHANGES` alike, and a forgotten card therefore costs a review;
  the author row is what stops the merge request disappearing from the digest.
- **Linear-aware completion:** at `In Review` or later, a linked MR with at least
  one *readable* approval stops notifying its remaining reviewers. If its issue is
  still exactly `In Review`, the author gets `move CHAIN-N forward in Linear`.
  Zero approvals or any `REQUESTED_CHANGES` verdict keep the ordinary GitLab flow.

A person who owes nothing is not listed. Without Linear, a digest with no
actions is not posted; with Linear enabled, the healthy `In Review` aggregate
is itself reportable and may produce a count-only digest.

When a team declares `linear_team_ids`, the message contains the aggregate
`Linear · In Review: N`; individual Linear issues are not duplicated as rows.
MR linkage prefers a valid identifier in the title and falls back to the source
branch, case-insensitively.

Linear is read while the digest payload is built. The payload is then persisted,
so a `slack_send` retry never reads Linear again. A Linear outage degrades an
otherwise available digest to `partial` and adds a visible warning; if GitLab
and Linear are both unavailable, no misleading digest is created.

The three Linear reads degrade separately, and the warning names the degradation
whose consequence the reader can see. When more than one fails the warning names
the most consequential and the log line carries all of them
(`count_known`, `links_known`, `gate_known`, plus every error).
Losing the board query removes the count entirely — *Partial data: Linear could
not be inspected.* — because an unknown count must never render as a zero.
Losing only the per-MR issue lookup keeps the count and says so — *Partial data:
Linear issue links could not be resolved; the In Review count is current.* — and
every reviewer falls back to the ordinary GitLab classification, so no merge
request is hidden by a link the service could not resolve. Losing only a team's
workflow order keeps both — *Partial data: Linear workflow order could not be
read; every linked merge request was treated as ready for review.* — which is the
one degradation that changes who the digest asks, so it is named rather than
folded into the others.

### Column order and the review gate

"Before `In Review`" is decided by the team's own board, not by a list of status
names in the service. Linear gives every workflow state a `type` whose order it
fixes itself — `triage` → `backlog` → `unstarted` → `started` → `completed` /
`canceled` — plus a `position` that orders states *within* one type. Only
`In Review` is ever named in configuration; everything else is whatever the team
called it.

`canceled` is graded as **before** review on purpose. Linear sorts it last so it
falls off the end of a board, but an open merge request on a cancelled card is
work that stopped, not work that finished review — its author needs to move the
card or close the merge request, and three reviewers do not need to read it.

A card contributes only its state **id**; the board contributes the order. That is
deliberate: the card's own copy of its status arrives from a different query, and a
missing or `null` position there is indistinguishable from a real `0`, which would
grade every column after `In Review` as "never offered" and silence a whole team's
reviewers. A column the board did not report — a page truncated below it, one
created since — is unorderable and therefore fails open.

`doctor` prints the resulting split per team, in board order (type first, then
position), which is the only way to answer "why did my merge request not reach a
reviewer". It is printed even when another team's mapping is broken. A column whose
`type` this build cannot order raises a warning on the same line: those cards fail
open — reviewers are notified exactly as before — which is safe but silent.

A merge request naming several valid Linear issues is graded only if they agree.
"First valid identifier in the title wins" is fine for naming a card, but a title
like `CHAIN-1 superseded by CHAIN-2 work` would otherwise let a cancelled duplicate
silence every reviewer and hand the author an instruction they cannot follow.

Every issue is graded against **its own** Linear team. A service team may map
several `linear_team_ids`, and each of them orders its columns independently.

### Approvals readability

Both completion rules read `ApprovedBy` together with `ApprovalsKnown`, because
"nobody approved" and "we could not ask" have to be different answers.

`GET /projects/:id/merge_requests/:iid/approvals` is available on **every** GitLab
tier — unlike approval *rules*, which are Premium — so a refusal here is a
**permissions** problem, exactly like `head_pipeline` above: the service account
needs at least Reporter on the project, and `merge_requests_access_level` must not
be restricted below it. While it is refused, every merge request reads as
unapproved and both completion rules go inert: reviewers keep being nudged after
approving, and, where Linear is configured, no author is asked to advance a card.

Nothing in a digest shows that, so it is reported three ways: `doctor`'s
`approvals visibility` check, the
`merge_requests_with_unknown_approvals_total{team}` gauge (any non-zero value means
some approvals were unreadable; a value that stays non-zero across slots means the
endpoint is refused rather than flaky), and one WARN per run naming both
consequences. The readiness gate does not depend on approvals, so it keeps working
through this.

---

## Schedules

| Schedule                   | What it does                                                     |
| -------------------------- | ---------------------------------------------------------------- |
| `review.scan_interval` (5m) | enqueues `scan`, which fans out one `scan_repo` per repository   |
| **each team's `digest.slots`** in its `digest.timezone` (default 09:00, 14:00, 17:30 Europe/Moscow) | enqueues one `digest` for that team |
| `jobs.cleanup_interval` (1h) | sweeps orphaned worktrees and stale mirrors under `review.workdir` |

Periodic jobs are inserted by the River **leader** only, so they fire once
across all replicas. `scan` and `cleanup` also run immediately on start (a fresh
replica should not idle for a whole interval, and a SIGKILL is exactly what
leaves worktrees behind). `digest` carries `RunOnStart` too, but it is guarded by
a same-day slot check, so a restart only fires a slot whose time has already
passed **today** and which has no run recorded yet — the hedge exists because
periodic jobs are leader-only and a leadership change must not skip a slot.

**The digest schedule is configuration** — `digest.slots`, `digest.timezone` and
the two skip lists, each overridable per team. The zone is
resolved from tzdata embedded in the binary (`internal/scheduler` imports
`time/tzdata`), so the schedule does not depend on the container's local time or
on the image shipping a zone database. The digest's `run_date` and slot are
resolved in that zone too — at 23:30 UTC the Moscow calendar day is already
tomorrow.

**Days with no digest** — `digest.skip_weekdays` and `digest.skip_dates`, both
empty by default and both overridable in `teams[].digest`:

```yaml
digest:
  skip_weekdays: [sat, sun]
  skip_dates: ["01-01", "01-02", "05-09", "2026-12-31"]
```

- A date is either **one day** (`2026-12-31`) or **the same day every year**
  (`05-09`). Prefer the annual form for holidays: a list of full dates expires
  every December and the failure is silent — the digest simply starts arriving on
  a holiday again.
- The calendar day is the one in the schedule's `timezone`. "Saturday" means
  Saturday where the team is.
- Nothing is *built* on a skipped day: the periodic job never fires, so a skipped
  day costs no GitLab requests rather than building a digest and withholding it.
  The `RunOnStart` reconciliation obeys the same rule, so a replica that starts
  on a Saturday morning does not send the digest the skip list exists to
  suppress.
- **A team's list replaces the global one; it cannot subtract from it.** An empty
  list means "inherit", so a team that must ignore a global skip carries its own.
  The two lists inherit independently — a support team that works weekends can
  drop the weekend rule and keep the company holidays.
- `ai-reviewer digest` run by hand **does** send on a skipped day: somebody typed
  the command, and the skip list describes the schedule. It says so in a note
  first. `digest --dry-run` prints the same note, which is what makes "why is the
  channel quiet today" answerable without reading the config.
- Skipping all seven weekdays is a legitimate way to say "this team wants no
  scheduled digest" — there is no other switch for it — and `doctor` reports that
  team's schedule as a **warning** naming the consequence, rather than leaving it
  to be worked out from seven weekday names.
- The skip applies to the *schedule*, not to delivery: a digest built on Friday
  whose `slack_send` retries into Saturday is still delivered. Losing it would be
  the wrong reading of "no digest on Saturday".

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

## Stopping the service

The first signal (`SIGINT`, `SIGTERM`, `SIGQUIT`) starts a graceful shutdown; **the
second one exits immediately**, abandoning whatever is in flight, with status 1.
The log says so at the moment the first arrives:

```text
info  shutdown signal received: draining, send it again to exit immediately  {"signal": "interrupt"}
```

What the graceful path waits for is publication and Slack delivery — network calls
measured in seconds. **Reviews are not drained.** A review takes 4–10 minutes and
`jobs.drain_timeout` is 60s, so waiting could never let one finish; it would only
hold the shutdown open for the full window while continuing to pay for output that
is discarded when the window closes. In-flight reviews are therefore cancelled the
moment the shutdown starts, their attempt rows are discarded rather than counted
against the merge request, and the next scan pass enqueues the same head SHA again.

So a normal stop takes about as long as the slowest in-flight publish, and the
worst case is bounded by `jobs.drain_timeout` — not by the length of a review.

---

## Observability

The ops server is **opt-in**: mx ships it off, and
[`config.example.yaml`](../config.example.yaml) is what turns it on. A deployment
configured purely through the environment sets `AI_REVIEWER_OPS_ENABLED`,
`AI_REVIEWER_OPS_HEALTHY_ENABLED` and `AI_REVIEWER_OPS_METRICS_ENABLED` itself —
without them the service runs with no listener at all, and a `/livez` healthcheck
(the shipped compose file has one) never passes.

Once on, it exposes, on `ops.*.port` (10000 by default):

| Path           | Purpose                                              |
| -------------- | ---------------------------------------------------- |
| `/livez`       | liveness — service state only, 503 on a failed one   |
| `/readyz`      | readiness — service state **and** the health checks  |
| `/healthy`     | the health-check results alone, as JSON              |
| `/metrics`     | Prometheus                                           |
| `/debug/pprof` | profiler — **off by default**, see below             |

**Point probes at `/readyz`, not `/healthy`.** They are not the same check: mx's
`/healthy` returns only the health-checker poll map, so it ignores service state
entirely, while `/readyz` overlays the two and is the one that goes red while
Postgres is unreachable or a service is still starting. `/healthy` is useful for
reading *which* checker is unhappy — it answers with a name per checker — and
misleading as a gate. The shipped compose healthcheck uses `/livez`, which is the
right question for "should this container be restarted".

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
| `ai_reviewer_linear_requests_total{operation,result}`, `ai_reviewer_linear_request_duration_seconds{operation}` | Linear GraphQL health and latency |
| `ai_reviewer_linear_issues_in_review{team}` | last successfully collected Linear issue count |
| `ai_reviewer_digest_source_errors_total{team,source}` | source failures that degraded a digest |
| `merge_requests_scanned_total{team}`               | counter: open MRs inspected                    |
| `merge_requests_waiting_human_review_total{team}`, `merge_requests_with_changes_requested_total{team}`, `merge_requests_with_unresolved_threads_total{team}`, `merge_requests_with_conflicts_total{team}`, `merge_requests_with_failed_pipeline_total{team}` | per-team **gauges**, rewritten on every digest build — once per slot, not every scan, because the classification needs a whole team at once. A flat line between two slots is correct. The first two split the queue: waiting-on-reviewers versus waiting-on-authors |
| `merge_requests_linear_not_ready_total{team}` | gauge, same cadence: merge requests the readiness gate parked with their author because the card has not reached `In Review`. The mutually exclusive counterpart of `waiting_human_review`, so an empty review queue can be read against it. A number that stays up means the team routinely opens merge requests without moving the board |
| `merge_requests_with_unknown_approvals_total{team}` | gauge, same cadence: merge requests whose approvals GitLab refused to report. Zero is the only healthy value; a value that stays non-zero across slots means the endpoint is refused rather than flaky, and both Linear completion rules are inert |
| `linear_gate_ambiguous_total{team}` | **counter**: merge requests whose readiness gate was skipped because they named Linear issues at different statuses. Meant to be rare — the identifier match yields candidates, not identifiers, so an ordinary branch name can contribute a second issue when the workspace owns a team with that key |
| `slack_digest_runs_total{team,result}`, `slack_messages_sent_total`, `slack_send_errors_total`, `slack_resend_uncertain_total` | digest delivery |
| `slack_commands_total{team,scope,result}`         | in-chat commands; `scope` separates `/all` from `/my`, and an `error` is somebody who typed a command and got nothing back |
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

Watching `canceled` on its own is still useful, but it no longer says anything
about `jobs.drain_timeout`: reviews are cancelled deliberately when a shutdown
starts (see [Stopping the service](#stopping-the-service)). A rise means the
process is restarting often — deploys, OOM kills, a crash loop — and each restart
throws away whatever reviews were running, to be re-done from scratch by the next
scan.

---

## Troubleshooting

**Start with `ai-reviewer doctor`.** It covers most of what follows, and prints
no secret values, so its output is safe to paste into a ticket.

| Symptom                                            | Likely cause                                                                                             |
| -------------------------------------------------- | -------------------------------------------------------------------------------------------------------- |
| Nothing happens at all after `start` runs           | Both dry-run switches are off by default. Check `service.*_enabled`, and look for persisted `dry_run` rows |
| It reviews MRs though nobody enabled that          | Reviewing is `teams[].ai_review.enabled`, not `service.ai_review_publish_enabled` — the second only decides whether findings are posted. The `effective mode` line at startup names every team it is on for |
| Ctrl-C does nothing, the process will not stop     | Fixed: the process has one signal owner and the second signal force-exits. If a build still ignores it, `kill -9`; reviews are re-enqueued by the next scan either way |
| `config` check fails listing several problems       | Fail-fast validation. It reports every problem at once — fix them together                                |
| `river schema` check fails                          | `ai-reviewer migrations up` was never run against this database                                          |
| `migrations` check reports pending                  | A replica started with `migrate_on_start: false`; run `ai-reviewer migrations up`, or turn it back on     |
| Reviews never start, `readyz` red                   | Postgres unreachable. The service is designed to stop rather than proceed blind                          |
| "pipeline failed" never appears in the digest       | The token cannot see pipelines — the service account needs at least Reporter (§ GitLab permissions)      |
| Digest posts nothing, token looks fine              | The bot is not a member of the channel — `doctor` checks this per channel. If that check instead reports `missing_scope`, the token cannot read the channel at all: add `channels:read` (`groups:read` for a private one). Delivery is unaffected by those two; membership is what it needs |
| Everyone in the digest appears without a mention    | The token cannot read the directory. `doctor` says which scope is missing (`users:read`, then `users:read.email`); GitLab also rarely exposes emails, so add `slack.user_map` overrides |
| One person appears as plain text while others ping  | Ambiguous match, a deactivated Slack account, or a `user_map` entry that did not resolve — the log says which. Deactivated accounts are excluded on purpose |
| A `user_map` entry seems to be ignored              | It did not resolve, which looks identical to having no entry. `doctor` resolves every entry and prints what it became |
| A review completes with 0 findings                  | `review complete` now carries a `suppressed=` breakdown; a large `not_in_diff` means the model keeps commenting on files the MR does not touch |
| `held back by the failure backoff` for a healthy MR  | Fixed: a running review is reported as `review already in progress`. A WARN now means real failures |
| `claude auth` check fails                           | The mode and the credential disagree. `oauth-token` needs `AI_REVIEWER_CLAUDE_CODE_OAUTH_TOKEN`; `existing-login` will not work in a container |
| Reviewer states look coarse                         | GraphQL is disabled or unsupported; the REST heuristic is in use. `doctor` says which                    |
| Linear section is missing                           | The team declares no `linear_team_ids`, or Linear failed — an empty board still renders `Linear · In Review: 0`, so an absent line never means "no issues". A failure also adds a warning and marks the run `partial`. Run `doctor` to validate the key, UUIDs and workflow state |
| A merge request reaches nobody's review queue       | Its Linear card has not reached `In Review`, so the readiness gate parked it with its author — look for the `move CHAIN-N to In Review` row under the author, and check `merge_requests_linear_not_ready_total`. `doctor`'s `linear review gate` line prints which columns count as "before" for that team and board |
| A card is behind `In Review` but reviewers are still asked | The gate refused to grade it. Either the board could not be ordered (the digest carries a `Partial data: a Linear board could not be ordered` warning, and `merge_requests_linear_not_ready_total` goes absent), or the merge request names several Linear issues at different statuses — check `linear_gate_ambiguous_total` and the `references Linear issues at different statuses` log line |
| Reviewers are still nudged after somebody approved  | `GET /approvals` is unreadable, so the approval is invisible to the service and both completion rules are inert. It is available on every GitLab tier, so the cause is access: give the service account at least Reporter and check `merge_requests_access_level`. Confirm with `doctor`'s `approvals visibility` check or the `merge_requests_with_unknown_approvals_total` gauge |
| A review job runs but publishes nothing             | It ran as a dry run. Re-run with `ai-reviewer review <ref> --publish --wait`                              |
| `scan --team X` reports it was folded in            | A full scan was already in flight; scan uniqueness is by kind. Wait for the next pass                    |
| A finding lost its line anchor                      | GitLab rejected the stored position; the finding is posted as an unpositioned discussion rather than lost |
| The same MR keeps failing                           | The 15m/1h/6h backoff is in effect; after the fourth failure it stops until a new head SHA               |
| Worktrees accumulate under `review.workdir`         | The cleanup job runs hourly and on start; check the `cleanup` job in `river_job`                          |

---

[← back to the README](../README.md)
