# Installation

Everything needed to get ai-reviewer running: the database, the three external
accounts it talks to, and the two supported ways to deploy it.

---

## PostgreSQL and migrations

PostgreSQL holds the service's **operational state**, not a copy of GitLab:
which SHA was reviewed and what came of it, which findings were published and
under which note id, digest runs and their parts, and River's queue and leader
election.

- **Minimum supported: PostgreSQL 16.** Production targets **17**. Local
  development deliberately runs **18** (`dev/docker/docker-compose.yml`), so
  version-dependent behaviour is exercised against the newer server too.
- Every table uses `id uuid PRIMARY KEY DEFAULT uuidv7()`. `uuidv7()` became a
  built-in only in PostgreSQL 18, so the single migration **`0001_init`**
  installs a pl/pgSQL shim — but **only when the server has none**. The guard is
  a capability check against `pg_proc`, not a version comparison, so exactly one
  `uuidv7()` exists on any server: the built-in on 18+, the shim on 16/17. The
  `DEFAULT` expressions are left unqualified so each server binds to whichever
  it has. (Note that binding is resolved once, at `CREATE TABLE` time, and
  stored as a parse tree with a fixed OID — it is not re-resolved per `INSERT`.)
- The shim matches the built-in's ordering, not just its layout: both spend the
  12 `rand_a` bits on sub-millisecond clock precision (RFC 9562 method 3), so
  ids generated inside one millisecond are still strictly increasing. Without
  that, B-tree insert locality and `ORDER BY created_at, id` tiebreaks would
  behave differently on dev (18) and production (17).

```bash
ai-reviewer migrations up      # application schema, then River's own
ai-reviewer migrations down    # roll back the last APPLICATION migration only
ai-reviewer migrations create --name add_something
```

`up` accepts `--dsn` (or `$AI_REVIEWER_POSTGRES_DSN`) and, when given one,
**skips config validation** — an init container that has a database URL and no
GitLab or Slack credentials can still apply the schema. Without `--dsn` the DSN
is assembled from `postgres.*`.

`down` deliberately touches only the application's migrations. River versions
its own tables, and rolling those back under a running service would discard
queued jobs.

**The service migrates itself.** `postgres.migrate_on_start` is **on by default**,
so a starting process applies the application schema and then River's before it
claims any work — you do not run `migrations up` from CI, a release job or an
init container, and a fresh cluster needs no separate step.

That holds with any number of replicas: a session-level `pg_advisory_lock` is
held across **both** migrators for the whole apply loop, so replicas starting
together serialise and every one after the first finds nothing pending. The lock
spans both because River's migrator takes none of its own — without it, two
processes would both read an empty `river_migration` and both run
`CREATE TABLE river_job`, and the loser would abort.

Set it to `false` only if something else owns the schema — a policy that forbids
workloads from altering databases, say. Then apply it yourself with
`ai-reviewer migrations up --dsn …`, and note that a replica will start against
an out-of-date schema rather than fix it.

---

## GitLab permissions

Create a **project or group access token / personal access token for a service
account** with:

- **scope `api`** — the service reads merge requests, diffs, discussions,
  approvals and pipelines, and writes discussions and notes;
- **role at least `Reporter`** on every configured repository.

Reporter is not a formality. `head_pipeline` is only returned to a token allowed
to see the project's pipelines, so with a lower role the "❌ pipeline failed"
line simply never appears in a digest — nothing errors, nothing retries, the
signal is just absent. `ai-reviewer doctor` checks this explicitly: for one open
MR in each repository it looks at whether `head_pipeline` came back and warns if
it did not.

The token is sent in the `PRIVATE-TOKEN` header, never in a URL. Agent-mode
clones authenticate through `http.extraHeader` supplied in the environment, so
the token never lands in a clone URL, in the mirror's git config, or in `argv`.

GraphQL is used for exactly one thing: each reviewer's precise review state
(`reviewState`). Self-managed instances too old to answer it fall back to a REST
heuristic automatically; `doctor` reports which of the two you are on.

---

## Linear setup (optional)

Linear is a read-only digest source. Create a personal API key in Linear under
**Settings → Security & access → Personal API keys**. Prefer a technical account
that can access only the teams the service needs. Put the key in
`AI_REVIEWER_LINEAR_API_KEY` (or the Vault key with the same name), not in the
committed YAML. For the bundled Docker Compose deployment, uncomment the
corresponding line in `.env` (copied from `.env.example`).

For every application team that should use Linear-aware MR classification:

1. Open the corresponding team in Linear.
2. Press `Cmd+K` / `Ctrl+K` and select **Copy model UUID**.
3. Add the copied UUID to that application's `linear_team_ids` list.

```yaml
linear:
  api_key: "" # AI_REVIEWER_LINEAR_API_KEY

teams:
  - name: payments
    slack_channel: C012345678
    linear_team_ids:
      - "your-linear-team-uuid"
    repositories: [backend/payments]
```

The service matches an identifier such as `CHAIN-184` in the MR title, then the
source branch, without regard to case. One GitLab approval completes review for
a linked issue unless any reviewer has `REQUESTED_CHANGES`; an approved issue
still in `In Review` becomes an action for the MR author to move it forward.
With zero approvals or no matching issue, the ordinary GitLab reviewer flow is
kept. Run `ai-reviewer doctor`: it authenticates the key, resolves every UUID
and verifies that each Linear team has exactly one matching workflow status. If
`linear_team_ids` is absent everywhere, Linear is skipped and no key is required.

## Slack setup

Create a Slack app with a bot token and these **scopes**:

| Scope              | Why                                                        |
| ------------------ | ---------------------------------------------------------- |
| `users:read`       | load the workspace directory for user matching             |
| `users:read.email` | without it `users.list` omits email — the strongest match  |
| `chat:write`       | post the digest                                            |

**Invite the bot to every channel** listed in `teams[].slack_channel`. The most
common failure is a perfectly valid token whose `auth.test` passes while every
post returns `not_in_channel`; `doctor` checks membership per channel for
exactly this reason.

The workspace directory is cached **in process memory only** (`slack.directory_ttl`,
15m by default) behind a singleflight, so the two teams whose digests fire at
09:00 share one `users.list` call. Nothing about Slack is persisted; after a
restart the cache is simply rebuilt.

Delivery always goes through the queue: `digest` builds the Block Kit payload
and stores it, and a separate `slack_send` job per message part performs the
`chat.postMessage`. A message too large for Slack's limits is split into
numbered parts (`MR Digest — Payments (1/3)`) — never silently truncated — and
each part is its own row and its own job, so a failure on part 3 does not undo
parts 1 and 2.

---

## Claude authentication

Reviews shell out to the **Claude Code CLI**. Which credentials the subprocess
sees is decided deterministically by `llm.claude.auth.mode`, never inherited by
accident:

| mode             | `ANTHROPIC_API_KEY`      | `CLAUDE_CODE_OAUTH_TOKEN` | Use                                   |
| ---------------- | ------------------------ | ------------------------- | ------------------------------------- |
| `existing-login` | removed from the env     | removed                   | a developer machine already logged in |
| `oauth-token`    | **forcibly removed**     | set from config/Vault     | subscription credentials              |
| `api-key`        | set from config/Vault    | **forcibly removed**      | Anthropic API billing                 |

In every mode the conflicting provider variables (`CLAUDE_CODE_USE_BEDROCK`,
`CLAUDE_CODE_USE_VERTEX`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_BASE_URL`) are
stripped unless the operator opted back in through `llm.claude.extra_env` —
otherwise a stray variable on a node would quietly change your model and your
bill. The chosen mode's description is logged once at startup, never per call,
and never with values.

**OAuth token** (subscription):

```bash
claude setup-token          # on a machine with an interactive login
# put the result in Vault, or in AI_REVIEWER_CLAUDE_CODE_OAUTH_TOKEN
```

**API key**: create one in the Anthropic Console and set
`AI_REVIEWER_ANTHROPIC_API_KEY` with `mode: api-key`.

`existing-login` is not a container scenario: it requires an interactive browser
login and a `~/.claude` that survives restarts. In Docker or Kubernetes use
`oauth-token` or `api-key`.

> **A note on subscription credentials.** Claude subscription OAuth credentials
> are intended for use of Claude Code by the holder of that subscription.
> `ai-reviewer` neither collects nor proxies anyone else's credentials: it uses
> exactly the one set of credentials its operator configures. For an
> organisational or commercial deployment, check Anthropic's current terms and
> use an API key, or Team/Enterprise (or cloud-provider) authentication, as
> appropriate for your situation.

Every `claude` flag is config-driven (`llm.claude.{bin,model,permission_mode,
allowed_tools,extra_args}`) so the wrapper survives CLI changes. Cost accounting
is whatever the CLI reports: `ai_review_cost_usd_total` sums the
`total_cost_usd` field of each call's JSON envelope, and stays at zero when the
CLI does not report one.

---

## Docker Compose

[`docker-compose.yml`](../docker-compose.yml) is the whole stack: PostgreSQL 17
and the service. There is no third container — the service applies the schema
itself, as below.

```bash
cp .env.example .env    # the three tokens
cp config.example.yaml config.yaml    # your teams and their repositories
docker compose up -d
```

There is no migrations step: the service waits on the database's health check and
then applies the schema itself on the way up, its own migrations and then
River's, under one advisory lock.

Two volumes: `pgdata` for the database, and `work` for `review.workdir` — git
mirrors and per-review worktrees, which is why it must not be an image layer
(one mirror can be gigabytes). The `cleanup` job sweeps it.

`config.yaml` is what decides where inside that volume they land, and the image
does not overrule it: `config.example.yaml` sets `review.workdir: /work`, so the
Compose deployment uses `/work` itself. Remove the key and the schema default
`./data` resolves against the image's `WORKDIR` — `/work/data`, still inside the
same volume. Point the key at another path and mount `work` there instead. (The
image sets no `AI_REVIEWER_REVIEW_WORKDIR`, deliberately: the environment outranks
the file, so such a variable would silently make the key inert in the container.)

Everyday commands:

```bash
docker compose logs -f ai-reviewer
docker compose exec ai-reviewer ai-reviewer doctor
docker compose exec ai-reviewer ai-reviewer scan
docker compose down          # keep the volumes
docker compose down -v       # drop them too
```

**Memory.** Claude Code wants 4 GB+ per concurrent review and
`review.max_parallel` of them run in the one container, so `mem_limit` (10g) and
`review.max_parallel` (2) move together or not at all.

**Scaling.** More than one replica is safe. River makes one head SHA reviewable
once and elects a single leader for the periodic jobs, and concurrent `git fetch`
on one mirror — the other thing that would corrupt shared state — is serialised
per repository by a blocking Postgres advisory lock, so replicas may share the
`work` volume. What actually limits the count is memory: 4 GB+ per concurrent
review, `review.max_parallel` of them per replica.

## Docker

The image is Alpine-based and carries the Claude Code CLI, `git` and `ripgrep`
alongside the service binary. It runs as a non-root user (uid 10001) with a
writable `/work` — which is also its `WORKDIR`, so with no `review.workdir` in the
config the mirrors and worktrees go to `/work/data` — and contains **no
credentials**.

Its entrypoint is the binary and there is no default command, so the image is
the whole CLI rather than "the server image" — you name the command:

```bash
docker run --rm ai-reviewer:latest                  # help
docker run --rm ai-reviewer:latest doctor
docker run --rm ai-reviewer:latest review group/repo!42 --publish
docker run --rm ai-reviewer:latest start            # the long-running service
```

Compose builds it for you; build it by hand when you push to a registry:

```bash
docker build \
  --build-arg VERSION=$(git describe --tags --always) \
  --build-arg COMMIT=$(git rev-parse --short HEAD) \
  --build-arg DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
  -t ai-reviewer:latest .
```

```bash
docker run --rm \
  -e AI_REVIEWER_GITLAB_TOKEN=... \
  -e AI_REVIEWER_SLACK_TOKEN=... \
  -e AI_REVIEWER_CLAUDE_CODE_OAUTH_TOKEN=... \
  -e AI_REVIEWER_POSTGRES_HOST=... \
  -e AI_REVIEWER_POSTGRES_USERNAME=... \
  -e AI_REVIEWER_POSTGRES_PASSWORD=... \
  -v $PWD/config.yaml:/etc/ai-reviewer/config.yaml:ro \
  ai-reviewer:latest start --config /etc/ai-reviewer/config.yaml
```

**The agent is whatever the build pulled.** The image installs the current Claude
Code from Anthropic's apk repository, so rebuilding is how you move to a newer
one. The flip side is worth knowing: two builds of the same commit can ship
different agents, and `claude --version` in the container is what tells you which
you have.

Claude Code's self-updater is disabled in the image (`DISABLE_AUTOUPDATER=1`) —
the container is immutable, so the CLI version is the image's and `claude
--version` proves which. `USE_BUILTIN_RIPGREP=0` makes Claude Code use the
system `ripgrep`, because its bundled one is glibc-linked and cannot run on musl
and silently takes the `Grep` tool with it. Both variables are set on the
service process **and** named in the subprocess environment allowlist
(`internal/llm/auth.go`), which is what carries them into `claude`; the image and
that list have to move together.

The apk repository is signature-verified with a key the build fetches from the
same origin. That makes the check an integrity check — a corrupted or truncated
download fails — rather than an independent trust root, but it does keep `apk`
off `--allow-untrusted`. No key is committed to this repository.

Claude Code wants **4 GB+ of RAM**, and `review.max_parallel` of them can run
concurrently in one container. Size the memory limit accordingly.

---

[← back to the README](../README.md)
