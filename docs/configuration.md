# Configuration

Sources, in increasing priority: **YAML file(s) → environment (`AI_REVIEWER_*`)
→ Vault**. See [`config.example.yaml`](../config.example.yaml) for the full,
commented set; the default path is `config.yaml` in the working directory, and a
deployment configured purely through env and Vault needs no file at all.

Two things about this schema are worth knowing before you fight it:

- **Defaults do not come from struct tags.** `config.DefaultConfig()` is the
  single source of truth for defaults. Tag defaults fill every *zero* value
  after the file is read, which would flip an explicit `false` in your YAML back
  to `true`.
- **Every externally-settable field carries an explicit `env:` tag**, so the
  variable name is predictable. Without one, the name is derived by
  word-splitting the Go field path, and `gitlab.token` would be
  `AI_REVIEWER_GIT_LAB_TOKEN` (`GitLab` → `Git` + `Lab`).

## Environment variables

The name is `AI_REVIEWER_` + the field's `env:` tag. **Do not derive names by
hand from the YAML path** — most match, but the ones that do not are exactly the
ones you would get wrong. The table below is generated from the actual struct
tags in `internal/config/config.go`, and `TestEnvNamesRoundTrip` pins a sample of
them against the loader.

Secrets are marked ★ — those are also the fields the Vault plugin fetches.

| Variable                                          | YAML                                 | Default             |
| ------------------------------------------------- | ------------------------------------ | ------------------- |
| `AI_REVIEWER_CONFIG`                              | *(CLI flag `--config`)*              | `config.yaml`       |
| **Logging / ops**                                 |                                      |                     |
| `AI_REVIEWER_LOG_LEVEL`                           | `log.level`                          | `info`              |
| `AI_REVIEWER_LOG_FORMAT`                          | `log.format`                         | `json`              |
| `AI_REVIEWER_OPS_ENABLED`                         | `ops.enabled`                        | `true`              |
| `AI_REVIEWER_OPS_HEALTHY_PORT`                    | `ops.healthy.port`                   | `10000`             |
| `AI_REVIEWER_OPS_HEALTHY_LIVENESS_PATH`           | `ops.healthy.liveness_path`          | `/livez`            |
| `AI_REVIEWER_OPS_HEALTHY_READINESS_PATH`          | `ops.healthy.readiness_path`         | `/readyz`           |
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
| `AI_REVIEWER_REVIEW_WORKDIR`                      | `review.workdir`                     | `/work`             |
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

Note the two that break the pattern: the Claude credentials are
`AI_REVIEWER_CLAUDE_CODE_OAUTH_TOKEN` and `AI_REVIEWER_ANTHROPIC_API_KEY`, not
`AI_REVIEWER_LLM_CLAUDE_AUTH_*`. That is deliberate — the tag is the bare name
`claude` itself understands, so an operator moving from a shell to a container
recognises it. The unprefixed `CLAUDE_CODE_OAUTH_TOKEN` / `ANTHROPIC_API_KEY`
are **not** read from the service's own environment; they are constructed for
the `claude` subprocess only.

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
`AI_REVIEWER_SLACK_TOKEN`, `AI_REVIEWER_CLAUDE_CODE_OAUTH_TOKEN`,
`AI_REVIEWER_ANTHROPIC_API_KEY`, `AI_REVIEWER_POSTGRES_USERNAME`,
`AI_REVIEWER_POSTGRES_PASSWORD`.

Every resolved secret is registered with the redactor before anything can log
it, so it is masked in logs, in error text and in the `claude` subprocess's
output.

---

---

[← back to the README](../README.md)
