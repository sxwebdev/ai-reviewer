# Configuration

Sources, in increasing priority: **defaults → YAML file(s) → environment
(`AI_REVIEWER_*`) → Vault**. See [`config.example.yaml`](../config.example.yaml)
for the full, commented set; the default path is `config.yaml` in the working
directory, and a deployment configured purely through env and Vault needs no file
at all.

Two things about this schema are worth knowing before you fight it:

- **Defaults live in `default:` struct tags**, and a value you write in the YAML
  always wins — including a value that happens to be the zero one. Setting
  `graphql_enabled: false` against a `true` default keeps your `false`, because
  the loader tracks which keys the file actually contained rather than looking for
  empty fields.
- **Most environment variable names are derived from the field path, not spelled
  out.** `review.severity_threshold` is `AI_REVIEWER_REVIEW_SEVERITY_THRESHOLD`.
  The exceptions are acronyms, where the derivation splits words the way Go
  capitalises them: everything under `gitlab.` is tagged explicitly, because
  `GitLab` would otherwise become `GIT_LAB`. Use the table below rather than
  guessing.
- **`teams[].ai_review.enabled` is the switch that spends money**, and it is per
  team. `service.ai_review_publish_enabled` is not a second half of it: it decides
  only whether findings are posted, and a review that posts nothing costs exactly
  what a published one costs. The example config ships reviewing off for every
  team, and `start` logs which teams it is on for
  ([Operations](operations.md#dry-run-modes)).

## Environment variables

**Do not derive names by hand from the YAML path** — most match, but the ones that
do not are exactly the ones you would get wrong. The table below follows the actual
struct tags in `internal/config/config.go`, and for `log.*` and `ops.*` the tags of
the embedded mx configs (`logger.Config`, `launcher/ops.Config`) — which is also
where those defaults come from.

Secrets are marked ★ — those are also the fields the Vault plugin fetches. ⚠ marks
the three switches an env-only deployment has to set for the ops server to exist at
all; see the second note below.

Two defaults are worth reading before you trust the table:

- **`review.workdir` defaults to the relative `./data`**, so a local run works
  with no config. Inside the Docker image that relative path resolves against
  `WORKDIR`, which is `/work` — so a container given no `review.workdir` uses
  **`/work/data`**, inside the mounted volume. The image deliberately sets no
  `AI_REVIEWER_REVIEW_WORKDIR`: the environment outranks the file, so the
  variable would make the documented key inert in a container.
  [`config.example.yaml`](../config.example.yaml) sets `workdir: /work`, which is
  why the Compose install path lands on `/work` itself — change that key and the
  container follows it.
- **Every `ops.*.enabled` defaults to `false`.** Those defaults come from mx, not
  from this schema, and mx ships the ops server off — including the health checker
  and metrics. `config.example.yaml` turns all three on and Compose mounts it; a
  deployment configured purely through the environment must set
  `AI_REVIEWER_OPS_ENABLED`, `AI_REVIEWER_OPS_HEALTHY_ENABLED` and
  `AI_REVIEWER_OPS_METRICS_ENABLED` itself, or it comes up with no listener on
  10000 at all: no `/livez`, no `/readyz`, no `/metrics`
  ([Operations](operations.md#observability)).

| Variable                                          | YAML                                 | Default             |
| ------------------------------------------------- | ------------------------------------ | ------------------- |
| `AI_REVIEWER_CONFIG`                              | *(CLI flag `--config`)*              | `config.yaml`       |
| **Logging / ops**                                 |                                      |                     |
| `AI_REVIEWER_LOG_LEVEL`                           | `log.level`                          | `info`              |
| `AI_REVIEWER_LOG_FORMAT`                          | `log.format`                         | `json`              |
| `AI_REVIEWER_OPS_ENABLED`                         | `ops.enabled`                        | `false` ⚠           |
| `AI_REVIEWER_OPS_HEALTHY_ENABLED`                 | `ops.healthy.enabled`                | `false` ⚠           |
| `AI_REVIEWER_OPS_HEALTHY_PORT`                    | `ops.healthy.port`                   | `10000`             |
| `AI_REVIEWER_OPS_HEALTHY_LIVENESS_PATH`           | `ops.healthy.liveness_path`          | `/livez`            |
| `AI_REVIEWER_OPS_HEALTHY_READINESS_PATH`          | `ops.healthy.readiness_path`         | `/readyz`           |
| `AI_REVIEWER_OPS_HEALTHY_PATH`                    | `ops.healthy.path`                   | `/healthy`          |
| `AI_REVIEWER_OPS_METRICS_ENABLED`                 | `ops.metrics.enabled`                | `false` ⚠           |
| `AI_REVIEWER_OPS_METRICS_PORT`                    | `ops.metrics.port`                   | `10000`             |
| `AI_REVIEWER_OPS_METRICS_PATH`                    | `ops.metrics.path`                   | `/metrics`          |
| `AI_REVIEWER_OPS_PROFILER_ENABLED`                | `ops.profiler.enabled`               | `false`             |
| `AI_REVIEWER_OPS_PROFILER_PATH`                   | `ops.profiler.path`                  | `/debug/pprof`      |
| **Dry-run switches**                              |                                      |                     |
| `AI_REVIEWER_SERVICE_SLACK_SEND_ENABLED`          | `service.slack_send_enabled`         | `false`             |
| `AI_REVIEWER_SERVICE_AI_REVIEW_PUBLISH_ENABLED`   | `service.ai_review_publish_enabled`  | `false`             |
| **PostgreSQL**                                    |                                      |                     |
| `AI_REVIEWER_POSTGRES_HOST`                       | `postgres.host`                      | `localhost`         |
| `AI_REVIEWER_POSTGRES_PORT`                       | `postgres.port`                      | `5432`              |
| `AI_REVIEWER_POSTGRES_DATABASE`                   | `postgres.database`                  | `ai_reviewer`       |
| ★ `AI_REVIEWER_POSTGRES_USERNAME`                 | `postgres.username`                  | —                   |
| ★ `AI_REVIEWER_POSTGRES_PASSWORD`                 | `postgres.password`                  | —                   |
| `AI_REVIEWER_POSTGRES_SSL_MODE`                   | `postgres.ssl_mode`                  | `require`           |
| `AI_REVIEWER_POSTGRES_MIGRATE_ON_START`           | `postgres.migrate_on_start`          | `true`              |
| `AI_REVIEWER_POSTGRES_DSN`                        | *(flag `--dsn` of `migrations`)*     | —                   |
| **Jobs**                                          |                                      |                     |
| `AI_REVIEWER_JOBS_DRAIN_TIMEOUT`                  | `jobs.drain_timeout`                 | `60s`               |
| `AI_REVIEWER_JOBS_CLEANUP_INTERVAL`               | `jobs.cleanup_interval`              | `1h`                |
| `AI_REVIEWER_JOBS_QUEUES_DEFAULT`                 | `jobs.queues.default`                | `2`                 |
| `AI_REVIEWER_JOBS_QUEUES_PUBLISH`                 | `jobs.queues.publish`                | `1`                 |
| `AI_REVIEWER_JOBS_QUEUES_SLACK`                   | `jobs.queues.slack`                  | `1`                 |
| **GitLab**                                        |                                      |                     |
| `AI_REVIEWER_GITLAB_BASE_URL`                     | `gitlab.base_url`                    | —                   |
| ★ `AI_REVIEWER_GITLAB_TOKEN`                      | `gitlab.token`                       | —                   |
| `AI_REVIEWER_GITLAB_TIMEOUT`                      | `gitlab.timeout`                     | `30s`               |
| `AI_REVIEWER_GITLAB_GRAPHQL_ENABLED`              | `gitlab.graphql_enabled`             | `true`              |
| `AI_REVIEWER_GITLAB_MAX_ATTEMPTS`                 | `gitlab.max_attempts`                | `4`                 |
| `AI_REVIEWER_GITLAB_MAX_RETRY_AFTER`              | `gitlab.max_retry_after`             | `60s`               |
| `AI_REVIEWER_GITLAB_INSECURE_SKIP_VERIFY`         | `gitlab.insecure_skip_verify`        | `false`             |
| `AI_REVIEWER_GITLAB_CA_CERT_PATH`                 | `gitlab.ca_cert_path`                | —                   |
| **Linear (optional digest source)**               |                                      |                     |
| `AI_REVIEWER_LINEAR_ENDPOINT`                     | `linear.endpoint`                    | `https://api.linear.app/graphql` |
| ★ `AI_REVIEWER_LINEAR_API_KEY`                    | `linear.api_key`                     | —                   |
| `AI_REVIEWER_LINEAR_TIMEOUT`                      | `linear.timeout`                     | `15s`               |
| `AI_REVIEWER_LINEAR_MAX_ATTEMPTS`                 | `linear.max_attempts`                | `4`                 |
| `AI_REVIEWER_LINEAR_MAX_RETRY_AFTER`              | `linear.max_retry_after`             | `60s`               |
| **Slack**                                         |                                      |                     |
| ★ `AI_REVIEWER_SLACK_TOKEN`                       | `slack.token`                        | —                   |
| `AI_REVIEWER_SLACK_DIRECTORY_TTL`                 | `slack.directory_ttl`                | `15m`               |
| `AI_REVIEWER_SLACK_USER_MAP_<gitlab_username>`    | `slack.user_map.<gitlab_username>`   | —                   |
| **LLM**                                           |                                      |                     |
| `AI_REVIEWER_LLM_PROVIDER`                        | `llm.provider`                       | `claude-cli`        |
| `AI_REVIEWER_LLM_TIMEOUT`                         | `llm.timeout`                        | `15m`               |
| `AI_REVIEWER_LLM_CLAUDE_BIN`                      | `llm.claude.bin`                     | `claude`            |
| `AI_REVIEWER_LLM_CLAUDE_MODEL`                    | `llm.claude.model`                   | `sonnet`            |
| `AI_REVIEWER_LLM_CLAUDE_AUTH_MODE`                | `llm.claude.auth.mode`               | `existing-login`    |
| ★ `AI_REVIEWER_CLAUDE_CODE_OAUTH_TOKEN`           | `llm.claude.auth.oauth_token`        | —                   |
| ★ `AI_REVIEWER_ANTHROPIC_API_KEY`                 | `llm.claude.auth.api_key`            | —                   |
| `AI_REVIEWER_LLM_CLAUDE_PERMISSION_MODE`          | `llm.claude.permission_mode`         | `dontAsk`           |
| `AI_REVIEWER_LLM_CLAUDE_AGENT_MODE`               | `llm.claude.agent_mode`              | `true`              |
| `AI_REVIEWER_LLM_CLAUDE_ALLOWED_TOOLS`            | `llm.claude.allowed_tools`           | `Read(${worktree}/**),Grep(${worktree}/**),Glob(${worktree}/**)` |
| `AI_REVIEWER_LLM_CLAUDE_EXTRA_ARGS`               | `llm.claude.extra_args`              | —                   |
| `AI_REVIEWER_LLM_CLAUDE_EXTRA_ENV_<NAME>`         | `llm.claude.extra_env.<NAME>`        | —                   |
| `AI_REVIEWER_LLM_CLAUDE_PASSTHROUGH_ENV`          | `llm.claude.passthrough_env`         | —                   |
| **Review**                                        |                                      |                     |
| `AI_REVIEWER_REVIEW_SCAN_INTERVAL`                | `review.scan_interval`               | `5m`                |
| `AI_REVIEWER_REVIEW_MAX_PARALLEL`                 | `review.max_parallel`                | `2`                 |
| `AI_REVIEWER_REVIEW_MAX_COMMENTS`                 | `review.max_comments`                | `12`                |
| `AI_REVIEWER_REVIEW_SEVERITY_THRESHOLD`           | `review.severity_threshold`          | `medium`            |
| `AI_REVIEWER_REVIEW_PREFERRED_COMMENT_LANGUAGE`   | `review.preferred_comment_language`  | `auto`              |
| `AI_REVIEWER_REVIEW_WORKDIR`                      | `review.workdir`                     | `./data`            |
| `AI_REVIEWER_REVIEW_IGNORE_GLOBS`                 | `review.ignore_globs`                | see example         |
| `AI_REVIEWER_REVIEW_PIPELINE_MODE`                | `review.pipeline.mode`               | `standard`          |
| `AI_REVIEWER_REVIEW_PIPELINE_PASSES`              | `review.pipeline.passes`             | —                   |
| `AI_REVIEWER_REVIEW_PIPELINE_MAX_PARALLEL`        | `review.pipeline.max_parallel`       | `2`                 |
| `AI_REVIEWER_REVIEW_PIPELINE_VERIFY_MODE`         | `review.pipeline.verify_mode`        | `skeptic`           |
| `AI_REVIEWER_REVIEW_PIPELINE_VERIFY_MAX_FINDINGS` | `review.pipeline.verify_max_findings`| `24`                |
| `AI_REVIEWER_REVIEW_PIPELINE_VERIFIERS`           | `review.pipeline.verifiers`          | `go_build,go_vet,py_syntax` |
| `AI_REVIEWER_REVIEW_PIPELINE_COMPLETENESS`        | `review.pipeline.completeness`       | `auto`              |
| `AI_REVIEWER_REVIEW_CONTEXT_INCLUDE_FULL_FILES`   | `review.context.include_full_files`  | `true`              |
| `AI_REVIEWER_REVIEW_CONTEXT_MAX_FILE_LINES`       | `review.context.max_file_lines`      | `500`               |
| `AI_REVIEWER_REVIEW_CONTEXT_HUNK_WINDOW_LINES`    | `review.context.hunk_window_lines`   | `60`                |
| `AI_REVIEWER_REVIEW_CONTEXT_MAX_TOTAL_KB`         | `review.context.max_total_kb`        | `256`               |
| `AI_REVIEWER_REVIEW_CONTEXT_INCLUDE_COMMITS`      | `review.context.include_commits`     | `true`              |
| `AI_REVIEWER_REVIEW_CONTEXT_INCLUDE_DISCUSSIONS`  | `review.context.include_discussions` | `true`              |
| `AI_REVIEWER_REVIEW_CONTEXT_MAX_DISCUSSION_KB`    | `review.context.max_discussion_kb`   | `4`                 |
| `AI_REVIEWER_REVIEW_CONTEXT_PRIOR_REVIEW`         | `review.context.prior_review`        | `true`              |
| `AI_REVIEWER_REVIEW_CONTEXT_INTERDIFF_MAX_KB`     | `review.context.interdiff_max_kb`    | `32`                |
| `AI_REVIEWER_REVIEW_RISK_ENABLED`                 | `review.risk.enabled`                | `true`              |
| `AI_REVIEWER_REVIEW_RISK_HISTORY_COMMITS`         | `review.risk.history_commits`        | `500`               |
| `AI_REVIEWER_REVIEW_RISK_SENSITIVE_GLOBS`         | `review.risk.sensitive_globs`        | see defaults        |
| `AI_REVIEWER_REVIEW_COVERAGE_ENABLED`             | `review.coverage.enabled`            | `false`             |
| `AI_REVIEWER_REVIEW_COVERAGE_PROVIDERS`           | `review.coverage.providers`          | `go,node`           |
| `AI_REVIEWER_REVIEW_COVERAGE_TIMEOUT`             | `review.coverage.timeout`            | `5m`                |
| `AI_REVIEWER_REVIEW_COVERAGE_NODE_INSTALL`        | `review.coverage.node.install`       | `false`             |
| **Teams** (index `N` from 0)                      |                                      |                     |
| `AI_REVIEWER_TEAMS_N_NAME`                        | `teams[N].name`                      | —                   |
| `AI_REVIEWER_TEAMS_N_SLACK_CHANNEL`               | `teams[N].slack_channel`             | —                   |
| `AI_REVIEWER_TEAMS_N_REPOSITORIES`                | `teams[N].repositories` (comma-separated) | —              |
| `AI_REVIEWER_TEAMS_N_AI_REVIEW_ENABLED`           | `teams[N].ai_review.enabled`         | —                   |
| `AI_REVIEWER_TEAMS_N_LINEAR_TEAM_IDS`             | `teams[N].linear_team_ids` (comma-separated UUIDs) | —       |

Note the two that break the pattern: the Claude credentials are
`AI_REVIEWER_CLAUDE_CODE_OAUTH_TOKEN` and `AI_REVIEWER_ANTHROPIC_API_KEY`, not
`AI_REVIEWER_LLM_CLAUDE_AUTH_*`. That is deliberate — the tag is the bare name
`claude` itself understands, so an operator moving from a shell to a container
recognises it. The unprefixed `CLAUDE_CODE_OAUTH_TOKEN` / `ANTHROPIC_API_KEY`
are **not** read from the service's own environment; they are constructed for
the `claude` subprocess only.

`slack.user_map` takes a value in any of three forms, and the form decides how it
is resolved: a **user id** (`U…`/`W…`, upper case) answers offline and is the one
to reach for when `users.list` is unavailable; an **@handle or bare handle** and an
**email** are looked up in the directory like any other probe. Case is what tells
an id from a handle — Slack lower-cases handles — so `wendy` is a handle and
`W01WENDY` is an id. An override that resolves to nothing, or to more than one
person, is a configuration error rather than a quiet fall-back to name matching:
`doctor` resolves every entry and prints what each became.

Linear is enabled per application team by `teams[].linear_team_ids`. Get each
UUID by opening that team in Linear and choosing `Cmd/Ctrl+K` → **Copy model
UUID**. The example UUID from Linear's API documentation is not your team id.
When no team declares an id, the Linear client is not built and no key is
required. When at least one id is present, `linear.api_key` becomes required.
The digest shows the total `In Review` count and links an MR to a Linear issue
by identifier in the MR title, falling back to the source branch. Linked MRs
with at least one approval stop notifying their remaining reviewers; if the
issue is still `In Review`, the MR author is reminded to advance it. Zero
approvals, a missing issue, or any `REQUESTED_CHANGES` verdict keep the ordinary
GitLab flow. The status name is an invariant, not a setting: `doctor` rejects a
team without exactly one case-insensitive `In Review` workflow state.

`llm.claude.passthrough_env` is the seam for a deployment whose `claude` needs a
variable the built-in inheritance allowlist does not carry (see [Security
model](security.md)). It is a list of **names**, each of which may end in a
single `*` to admit a prefix, and it inherits the *parent's* value —
`llm.claude.extra_env` sets one instead. Neither can re-admit the credential or
provider variables the auth mode governs; a bare `*` is rejected at load with
"would inherit every variable, including the service's own secrets".

## Vault

Set the Vault bootstrap in the process environment or a `.env` file — these are
deliberately unprefixed, because a cluster shares them across services:

| Variable                 | Meaning                                    |
| ------------------------ | ------------------------------------------ |
| `VAULT_ENABLED`          | turn the plugin on                         |
| `VAULT_ADDR`             | Vault address                              |
| `VAULT_SECRET_PATH`      | KV path holding the secrets                |
| `VAULT_AUTH_KIND`        | `kubernetes` (default) or `token`          |
| `VAULT_KUBE_ROLE`        | Kubernetes auth role                       |
| `VAULT_KUBE_JWT_PATH`    | service-account token path                 |
| `VAULT_KUBE_MOUNT_PATH`  | auth mount, default `kubernetes`           |
| `VAULT_TOKEN`            | for `VAULT_AUTH_KIND=token`                |
| `VAULT_REFRESH_INTERVAL` | background refresh cadence, default `20s`  |

**The Vault key of a secret is its full environment-variable name, prefix
included** — store `gitlab.token` under the key `AI_REVIEWER_GITLAB_TOKEN`. The
plugin looks a field up by the name the env layer stamped on it, and that name
carries the prefix. Vault is registered last, so it has the final say over both
the file and the environment.

The Vault-backed fields are the ones marked ★ above: `AI_REVIEWER_GITLAB_TOKEN`,
`AI_REVIEWER_LINEAR_API_KEY`, `AI_REVIEWER_SLACK_TOKEN`, `AI_REVIEWER_CLAUDE_CODE_OAUTH_TOKEN`,
`AI_REVIEWER_ANTHROPIC_API_KEY`, `AI_REVIEWER_POSTGRES_USERNAME`,
`AI_REVIEWER_POSTGRES_PASSWORD`.

Every resolved secret is registered with the redactor before anything can log
it, so it is masked in logs, in error text and in the `claude` subprocess's
output.

---

---

[← back to the README](../README.md)
