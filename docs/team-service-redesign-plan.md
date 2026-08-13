# План перехода ai-reviewer: personal tool → team service

> Статус: **план, реализация не начата.** Дата: 2026-08-13.
> Ветка исходного состояния: `team-reviewer` (base `master`, HEAD `ff6a4d1`).
> Это **breaking redesign** существующего репозитория `github.com/sxwebdev/ai-reviewer`.
> Новый репозиторий не создаётся, второй продукт рядом не создаётся, обратная совместимость
> с personal mode не сохраняется.

---

## 0. TL;DR

Превращаем локальный однопользовательский инструмент в **self-hosted GitLab review agent для команд**:

```text
                  GitLab
                    │
                    ▼
               ai-reviewer  (N реплик, PostgreSQL + River)
                    │
       ┌────────────┼─────────────┐
       ▼            ▼             ▼
   AI review    Human review   MR hygiene
                   state      (threads/conflicts/
       │            │           failed pipelines)
       │            │             │
       └────────────┼─────────────┘
                    ▼
             Current MR state
              /            \
      GitLab comments      Slack digest
```

Ключевые сдвиги:

| Было                                    | Стало                                                                        |
| --------------------------------------- | ---------------------------------------------------------------------------- |
| «MR, где ревьюер — я»                   | Список repositories, сгруппированных по teams                                |
| SQLite `state.db` в `~/.ai-reviewer`    | **PostgreSQL** (pgx + pgxgen), схема в `sql/migrations`                      |
| Собственная job-очередь в SQLite        | **River** (`riverqueue/river`) поверх Postgres: unique jobs, retries, leader |
| Web UI + ручное approve/reject/publish  | Automation-first: валидные findings публикуются автоматически                |
| `serve` = локальный UI + воркер         | `serve` = production-сервис на `/mx` (health/metrics/graceful/ops)           |
| watch-daemon по назначенным MR          | River periodic job `scan` + periodic job `digest` (09:00 / 16:30 MSK)        |
| Токен в `config.yaml`                   | **YAML → env → Vault** (`xconfigvault`), тип `config.Secret`                 |
| `log/slog` + redacting slog-handler     | **`mx/logger` (zap)** + redaction как `zapcore.Core`-обёртка                 |
| Только REST GitLab                      | REST + **точечный GraphQL** (review state ревьюеров)                         |
| `replicas: 1`, распределённого лока нет | **N реплик**: конкуренцию снимает River (unique jobs + leader election)      |

**Отклонения от исходного ТЗ — согласованы явно:**

1. ТЗ требовало полного stateless и запрещало SQLite/PostgreSQL/Redis/persistent queue.
   Решение: **используем PostgreSQL + River**, потому что нужен корректный запуск ≥2 инстансов
   (дедупликация ревью одного MR и однократная отправка дайджеста). GitLab остаётся источником
   правды о самих MR; Postgres хранит операционное состояние сервиса.
2. ТЗ говорило «distributed lock НЕ нужен, replicas: 1». Решение: **N реплик**; взаимоисключение
   даёт River (unique jobs по `(project, iid, head_sha)`; periodic jobs вставляет только лидер).
3. ТЗ требовало хранить секреты только в env. Решение: **три источника — YAML, env, Vault**,
   по образцу `observe-ai` (`xconfig` + `xconfigvault`, тип `Secret` с `[redacted]` в `String()`).

Что **сохраняется по сути**: движок ревью (`internal/review`) — многопроходный fan-out, skeptic,
детерминированный валидатор, Go владеет позициями, verifier-плагины, ранжирование, фильтрация
binary/vendor/generated, scrubbing секретов; провайдер Claude Code CLI; GitLab-клиент v4.

---

## 1. Целевая концепция и границы

**Делает:**

1. периодически сканирует открытые не-draft MR в репозиториях сконфигурированных команд;
2. ставит в очередь и выполняет AI review при смене head SHA, публикует валидированные findings
   от имени service account;
3. классифицирует MR: кому из ревьюеров нужно действие, у каких MR unresolved threads,
   merge conflicts и **упавшие пайплайны**;
4. дважды в день (09:00 и 16:30 Europe/Moscow) шлёт per-team Slack digest с тегами реальных Slack-пользователей;
5. отдаёт health/readiness/metrics, корректно завершается по SIGTERM (drain River-джобов).

**Никогда не делает** (жёсткие инварианты):

- не approve и не merge MR;
- не resolve и не удаляет чужие треды/комментарии;
- не меняет reviewer assignments, labels, description MR;
- не даёт Claude CLI писать в репозиторий, пушить или обращаться к GitLab — комментарии
  публикует Go-код после валидации;
- не логирует секреты и не отправляет их в GitLab/Slack/metrics.

---

## 2. Инвентарь: что удаляется

### 2.1 Пакеты целиком

| Пакет               | Что это сейчас                                            | Судьба                   | Причина                                                           |
| ------------------- | --------------------------------------------------------- | ------------------------ | ----------------------------------------------------------------- |
| `internal/state`    | SQLite + миграции + репозитории (17 файлов, ~2 200 строк) | **Удалить**              | Замена — `internal/store` на Postgres/pgxgen                      |
| `internal/server`   | localhost web UI, HTMX-хендлеры (~2 000 строк)            | **Удалить**              | Personal UI, ручной approve/publish                               |
| `internal/ui`       | embed шаблонов и статики                                  | **Удалить**              | Вместе с web UI                                                   |
| `internal/index`    | FTS5-индексация worktree                                  | **Удалить**              | Зависела от SQLite FTS5; замена — поиск Claude Code               |
| `internal/jobs`     | Своя durable-очередь в SQLite + scheduler + worker        | **Переписать**           | Заменяется River-воркерами (тот же путь пакета, новое содержимое) |
| `internal/skills`   | Discovery Claude-скиллов для выбора в UI                  | **Удалить**              | Фича существует только ради per-review выбора в UI                |
| `screenshots/`      | Промо-скриншоты web UI                                    | **Удалить**              | Продукта с UI больше нет                                          |
| `internal/coverage` | Запуск тестов репозитория, LCOV/coverprofile              | **Сохранить, выключено** | Исполняет чужой код на общем хосте — см. §20.3                    |

### 2.2 Файлы/функции внутри сохраняемых пакетов

| Место                                                  | Что удалить                                                                                              |
| ------------------------------------------------------ | -------------------------------------------------------------------------------------------------------- |
| `internal/gitlab/endpoints.go`                         | `ListReviewerMRs` (`scope=reviews_for_me`); `CurrentUser` остаётся, но как проверка service account      |
| `internal/gitlab/draft_notes.go` + `API`               | Весь draft-notes слой (`CreateDraftNote`, `ListDraftNotes`, `PublishDraftNote`, `BulkPublishDraftNotes`) |
| `internal/gitlab/unconfigured.go`                      | Заглушка «GitLab не настроен, но UI должен стартовать»                                                   |
| `internal/service/sync.go`                             | Синхронизация назначенных MR                                                                             |
| `internal/service/publish.go`                          | `ConfirmPhrase`, `CreateDrafts`, `PublishDrafts` (workflow proposed→approved→drafted→published)          |
| `internal/service/finding.go`                          | Approve/reject/edit findings                                                                             |
| `internal/app/setup.go` (567 строк)                    | Setup-визард web UI, live-валидация токена, `ApplySetup`                                                 |
| `internal/app/worker.go`, `lifecycle.go`, `profile.go` | Personal-лайфцикл: `Serve`(UI), `RunDaemon`, `SyncOnce`, `printReport`                                   |
| `internal/app/logging.go`                              | slog-логгер (заменяется `mx/logger`)                                                                     |
| `internal/config/patch.go`, `template.go`, `schema.go` | Runtime-патч YAML из UI, форма настроек, `SettingsSchema` (460 строк)                                    |
| `internal/cli/commands.go`                             | Команды `sync`, `daemon`, алиас `start`, флаги `--auto-draft/--auto-publish/--open`                      |
| `internal/review/context.go`                           | `RelatedFile`-секция (питалась FTS) — заменяется агентным поиском                                        |
| `internal/llm/client.go`                               | Поле `Request.Skills` (вместе с `internal/skills`)                                                       |
| `internal/security/redaction.go`                       | slog-`Handler` → заменяется на `zapcore.Core`-обёртку (сам `Redactor` остаётся)                          |
| `go.mod`                                               | `modernc.org/sqlite`                                                                                     |

### 2.3 Конфигурационные секции к удалению

`app.*` (bind_host/port/open_browser/ui/data_dir), `storage.*`, `watch.*`, `index.*`,
`gitlab.username`, `review.create_drafts`, `review.auto_review`, `review.auto_draft`,
`review.auto_publish`, `review.default_mode`, `review.context.related_files`,
`llm.provider: anthropic-api` (реализации нет, остаётся единственный `claude-cli`).

### 2.4 Тесты к удалению

`internal/state/*_test.go` (8), `internal/server/*_test.go` (3), `internal/app/setup_test.go`,
`internal/jobs/worker_test.go`, `internal/index/indexer_test.go`, `internal/skills/skills_test.go`,
`internal/service/sync_test.go`, `internal/service/publish_test.go`, `internal/config/patch_test.go`,
`internal/config/schema_test.go`. Итого −17 файлов из 47.

---

## 3. Что сохраняется

| Компонент                                                                                                                                                         | Статус                                                    |
| ----------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------- |
| `internal/review`: passes, skeptic, validator, line_mapper, diff_parser/render, prompts, risk, completeness, verifiers, finding/fingerprint                       | **Сохранить.** Меняются только источники данных (см. §10) |
| `internal/llm` (Claude CLI wrapper, schema, json*extract, proc*\*)                                                                                                | **Сохранить**, добавить детерминированную сборку auth-env |
| `internal/gitlab` (клиент, retry, пагинация, ref-парсер, position)                                                                                                | **Сохранить**, расширить REST + добавить GraphQL (см. §9) |
| `internal/git` (mirror + worktree, http.extraHeader-аутентификация)                                                                                               | **Сохранить**, переориентировать на ephemeral-каталог     |
| `internal/security` (Redactor, Mask, Truncate)                                                                                                                    | **Сохранить**, сменить точку интеграции slog → zap        |
| `internal/toolchain`                                                                                                                                              | **Сохранить**                                             |
| `internal/version`, `cmd/ai-reviewer`                                                                                                                             | **Сохранить**                                             |
| Тесты движка (validator, line*mapper, pipeline, skeptic, prompts, diff*_, risk, verifier_, completeness, identifiers, suppressed, reflect, orchestrator, context) | **Сохранить** (адаптировать сигнатуры)                    |

---

## 4. Целевая структура

```text
cmd/ai-reviewer/
  main.go            — bootstrap: логгер, конфиг, mx launcher
  migrations.go      — команда migrations (app-миграции + rivermigrate)

sql/
  migrations/        — 0001_*.up.sql / .down.sql (embed через sql/embed.go)
  queries/           — SQL для sqlc/pgxgen по сущностям
  pgxgen.yaml        — конфигурация генерации моделей и репозиториев

internal/
  cli/          urfave/cli v3: serve, scan, review, digest, doctor, migrations
  app/          composition root: конфиг → клиенты → сервисы → mx launcher
  config/       xconfig-схема (teams/gitlab/slack/llm/review/postgres/ops/log), Secret, Vault
  domain/       Snapshot, Team, классификаторы (чистый пакет, без I/O)
  store/        pgxpool + сгенерированные pgxgen-репозитории + обёртки транзакций
  models/       сгенерированные pgxgen-модели
  jobs/         River: client-сервис, воркеры (scan / review / digest / cleanup), periodic
  scheduler/    Daily-расписание 09:00 / 16:30 Europe/Moscow (river.PeriodicSchedule)
  gitlab/       REST v4 + GraphQL + маркеры + публикация findings
  slack/        API-клиент (users.list, chat.postMessage, auth.test) + Block Kit builder
  match/        GitLab user → Slack user (matcher + кэш в Postgres + ephemeral индексы)
  git/          ephemeral mirror + worktree
  llm/          Client interface + Claude CLI + ClaudeAuth (детерминированный env)
  review/       движок (без изменений по сути)
  service/      review service (оркестрация одного MR) + digest service
  metrics/      объявление прометей-метрик (default registry, отдаёт mx ops)
  security/     Redactor + zapcore-обёртка
  toolchain/    поиск корней модулей / классификация тестовых путей
  version/
```

Зависимости строго вниз: `cli → app → {jobs, service} → {store, gitlab, slack, git, llm, review, domain} → security`.
`domain` не импортирует инфраструктуру — там живут классификаторы, покрытые тестами.

---

## 5. Хранилище: PostgreSQL + pgxgen

### 5.1 Роль базы

Postgres хранит **операционное состояние сервиса**, а не копию GitLab:

- какой head SHA каждого MR уже отревьюен и с каким результатом;
- какие findings опубликованы (для дедупликации и аудита);
- журнал прогонов дайджеста;
- кэш соответствий GitLab-пользователь → Slack-пользователь;
- очередь и состояние джобов (таблицы River).

GitLab остаётся источником правды о самих MR (состояние, дифф, треды, конфликты) — база их не дублирует.

### 5.2 Схема (`sql/migrations`)

```sql
-- 0001_reviews.up.sql
CREATE TABLE mr_reviews (
    id             uuid PRIMARY KEY DEFAULT uuidv7(),
    project_id     bigint      NOT NULL,
    project_path   text        NOT NULL,
    team           text        NOT NULL,
    mr_iid         bigint      NOT NULL,
    head_sha       text        NOT NULL,
    base_sha       text        NOT NULL DEFAULT '',
    start_sha      text        NOT NULL DEFAULT '',
    status         text        NOT NULL,          -- succeeded | failed | dry_run
    published      boolean     NOT NULL DEFAULT false,
    findings_count int         NOT NULL DEFAULT 0,
    risk_level     text        NOT NULL DEFAULT '',
    summary        text        NOT NULL DEFAULT '',
    pipeline_json  jsonb       NOT NULL DEFAULT '{}',
    risk_json      jsonb       NOT NULL DEFAULT '{}',
    cost_usd       numeric(12,6) NOT NULL DEFAULT 0,
    duration_ms    bigint      NOT NULL DEFAULT 0,
    error          text        NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now()
);
-- «этот SHA уже успешно отревьюен» — единственный запрос горячего пути
CREATE UNIQUE INDEX mr_reviews_success_uniq
    ON mr_reviews (project_id, mr_iid, head_sha)
    WHERE status <> 'failed';

CREATE TABLE mr_findings (
    id           uuid PRIMARY KEY DEFAULT uuidv7(),
    review_id    uuid NOT NULL REFERENCES mr_reviews(id) ON DELETE CASCADE,
    project_id   bigint NOT NULL,
    mr_iid       bigint NOT NULL,
    fingerprint  text   NOT NULL,
    severity     text   NOT NULL,
    category     text   NOT NULL,
    file_path    text   NOT NULL,
    title        text   NOT NULL,
    body         text   NOT NULL,
    position_json jsonb NOT NULL DEFAULT '{}',
    pass         text   NOT NULL DEFAULT '',
    verification text   NOT NULL DEFAULT '',
    note_id      bigint,                 -- id опубликованной заметки GitLab (NULL в dry-run)
    published_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);
-- дедуп по MR: один и тот же finding не публикуется повторно на новых SHA
CREATE UNIQUE INDEX mr_findings_fp_uniq ON mr_findings (project_id, mr_iid, fingerprint);

-- 0002_digest.up.sql
CREATE TABLE digest_runs (
    id          uuid PRIMARY KEY DEFAULT uuidv7(),
    team        text        NOT NULL,
    slot        text        NOT NULL,       -- '09:00' | '16:30'
    run_date    date        NOT NULL,       -- дата в Europe/Moscow
    status      text        NOT NULL,       -- sent | dry_run | failed | partial
    messages    int         NOT NULL DEFAULT 0,
    mr_count    int         NOT NULL DEFAULT 0,
    error       text        NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX digest_runs_slot_uniq ON digest_runs (team, run_date, slot);

-- 0003_slack_users.up.sql  (кэш матчинга; переживает рестарт, обновляется по TTL)
CREATE TABLE slack_user_links (
    gitlab_user_id  bigint PRIMARY KEY,
    gitlab_username text NOT NULL,
    slack_user_id   text NOT NULL DEFAULT '',
    match_kind      text NOT NULL,       -- email | username | name | manual | ambiguous | not_found
    updated_at      timestamptz NOT NULL DEFAULT now()
);
```

Плюс собственные таблицы River (`river_job`, `river_leader`, `river_queue`, `river_client*`),
применяемые через `rivermigrate` — своих миграций для них не пишем.

### 5.3 pgxgen

`sql/pgxgen.yaml` по образцу `observe-ai`: генерация моделей в `internal/models`
и per-table CRUD-репозиториев в `internal/store/repos/*`, `sql_package: pgx/v5`,
`type_overrides` для `jsonb → dbtypes.JSON` и `uuid → uuid.UUID`.
Ручные запросы (например «последнее успешное ревью по MR», «fingerprints по MR») —
в `sql/queries/<entity>/*.sql`.

### 5.4 Миграции

```bash
ai-reviewer migrations up      --dsn "$DSN"   # app-миграции + rivermigrate up
ai-reviewer migrations down    --dsn "$DSN"
ai-reviewer migrations create  -p ./sql/migrations -name add_x
```

В production миграции запускаются **init-контейнером**, не на старте приложения
(иначе гонка реплик). Локально — `postgres.migrate_on_start: true`.

---

## 6. Очередь и планировщики: River

### 6.1 Почему River закрывает конкуренцию

- **unique jobs** — `AnalyzeArgs`-подобный `ReviewArgs{ProjectID, MRIID, HeadSHA}` с тегом
  `river:"unique"` и `ByState(in-flight)`: два инстанса физически не могут ревьюить один и тот же
  SHA одновременно, а повторная постановка — no-op;
- **periodic jobs вставляет только выбранный лидер** (`river_leader`): дайджест в 09:00 отправляется
  ровно один раз при любом числе реплик;
- **durable + retries + graceful drain** из коробки, ручных advisory-локов не требуется;
- при падении реплики её джобы переподхватываются живыми (rescue), шардинг не нужен.

### 6.2 Джобы

| Kind      | Триггер                           | Очередь   | Уникальность                            | MaxAttempts | Timeout |
| --------- | --------------------------------- | --------- | --------------------------------------- | ----------- | ------- |
| `scan`    | periodic `review.scan_interval`   | `default` | по kind (in-flight)                     | 3           | 10m     |
| `review`  | из `scan` и из CLI `review`       | `review`  | `ByArgs` (project_id, mr_iid, head_sha) | **1**       | 30m     |
| `digest`  | periodic `Daily{09:00,16:30 MSK}` | `default` | `ByArgs` (team, slot, date)             | 3           | 10m     |
| `cleanup` | periodic 1h                       | `default` | по kind                                 | 1           | 5m      |

`review.MaxAttempts = 1` осознанно: упавшее ревью уже сожгло токены, автоповтор утраивает расход;
следующий `scan` всё равно снова поставит джобу, если SHA так и не отревьюен
(тот же приём, что в `observe-ai` для `analyze`).

`cleanup` удаляет осиротевшие worktree'ы в `review.workdir` (после SIGKILL) и старые mirror'ы.

Размер пула воркеров очереди `review` = `review.max_parallel`.

### 6.3 Расписание дайджеста

`river.PeriodicSchedule` — это интерфейс `{ Next(current time.Time) time.Time }` (проверено в
`river@v0.40.0/periodic_job.go`), поэтому кастомное расписание подключается напрямую:

```go
// internal/scheduler/daily.go
type Daily struct {
    Times []Clock          // {9,0}, {16,30}
    Loc   *time.Location    // Europe/Moscow
}
func (d Daily) Next(current time.Time) time.Time
```

09:00 и 16:30 — продуктовое требование, в конфиг не выносятся. Таймзона задаётся явно
(`time.LoadLocation("Europe/Moscow")`), локальная TZ контейнера не используется;
в `main` добавляется `import _ "time/tzdata"`, чтобы база таймзон была вшита в бинарь.

`digest_runs` с уникальным индексом `(team, run_date, slot)` даёт второй слой идемпотентности
и журнал для отладки/метрик.

---

## 7. Конфигурация (`/xconfig` + Vault)

### 7.1 Источники и приоритет

По образцу `observe-ai`: `YAML → env (AI_REVIEWER_*) → Vault`. Vault (плагин `xconfigvault`)
регистрируется последним и имеет максимальный приоритет; поля берутся из Vault по тегу `vault:"true"`.
Бутстрап-конфиг самого Vault читается из `.env`/env: `VAULT_ENABLED`, `VAULT_ADDR`,
`VAULT_SECRET_PATH`, `VAULT_AUTH_KIND` (`kubernetes`|`token`), `VAULT_KUBE_ROLE`,
`VAULT_KUBE_JWT_PATH`, `VAULT_KUBE_MOUNT_PATH`, `VAULT_TOKEN`, `VAULT_REFRESH_INTERVAL`.

Секретные поля объявляются типом `config.Secret` (копируем паттерн `observe-ai`:
`String()`/`GoString()`/`MarshalJSON`/`MarshalYAML` возвращают `[redacted]`, реальное значение
только через `Unmask()`), плюс теги `secret:"true" vault:"true"`:

```go
type GitLabConfig struct {
    BaseURL string        `yaml:"base_url" validate:"required,url" usage:"GitLab base URL"`
    Token   Secret        `yaml:"token" secret:"true" vault:"true" validate:"required" usage:"PAT service account (scope api)"`
    Timeout time.Duration `yaml:"timeout" default:"30s"`
}
```

Все резолвленные секреты дополнительно регистрируются в `security.RegisterSecret`, поэтому
маскируются и в тексте ошибок, и в выводе subprocess claude, и в логах.

### 7.2 Целевой `config.yaml`

```yaml
log:
  level: info
  format: json

ops: # mx ops: /livez, /readyz, /metrics, /debug/pprof
  enabled: true
  healthy: { enabled: true, port: "10000" }
  metrics: { enabled: true, port: "10000" }

service:
  slack_send_enabled: false # dry-run по умолчанию
  ai_review_publish_enabled: false # dry-run по умолчанию

postgres:
  host: localhost
  port: "5432"
  database: ai_reviewer
  username: "" # env / Vault
  password: "" # env / Vault
  ssl_mode: require
  migrate_on_start: false # в проде миграции из init-контейнера

jobs:
  drain_timeout: 60s # graceful drain River на shutdown
  review_queue_size: 2 # = review.max_parallel

gitlab:
  base_url: https://gitlab.company.ru
  token: "" # env AI_REVIEWER_GITLAB_TOKEN / Vault
  timeout: 30s
  graphql_enabled: true # точный review state ревьюеров

slack:
  token: "" # env AI_REVIEWER_SLACK_TOKEN / Vault
  user_map: {} # необязательный override: gitlab_username → SLACK_USER_ID

llm:
  provider: claude-cli
  timeout: 15m
  claude:
    bin: claude
    model: sonnet
    auth:
      mode: oauth-token # existing-login | oauth-token | api-key
      oauth_token: "" # env AI_REVIEWER_LLM_CLAUDE_AUTH_OAUTH_TOKEN / Vault
      api_key: "" # env AI_REVIEWER_LLM_CLAUDE_AUTH_API_KEY / Vault
    permission_mode: dontAsk
    agent_mode: true
    allowed_tools:
      [
        Read,
        Grep,
        Glob,
        "Bash(git diff *)",
        "Bash(git log *)",
        "Bash(git show *)",
      ]

review:
  scan_interval: 5m
  max_parallel: 2
  max_comments: 12
  severity_threshold: medium
  preferred_comment_language: auto
  workdir: /work # ephemeral
  ignore_globs:
    [vendor/**, node_modules/**, dist/**, build/**, "*.pb.go", "*.min.js"]
  pipeline:
    mode: standard
    max_parallel: 2
    verify_mode: skeptic
    verify_max_findings: 24
    verifiers: [go_build, go_vet, py_syntax]
    completeness: auto
  context:
    include_full_files: true
    max_file_lines: 500
    hunk_window_lines: 60
    max_total_kb: 256
    include_commits: true
    include_discussions: true
    max_discussion_kb: 4
    prior_review: true
    interdiff_max_kb: 32
  risk:
    enabled: true
    history_commits: 500
  coverage:
    enabled: false # исполняет код репозиториев — явный opt-in

teams:
  - name: payments
    slack_channel: C012345678
    ai_review: { enabled: true }
    repositories: [backend/payments, backend/billing, frontend/checkout]

  - name: platform
    slack_channel: C987654321
    ai_review: { enabled: true }
    repositories: [platform/auth, platform/gateway]
```

Загрузка — как в `observe-ai`: `xconfig.Load` с `WithEnvPrefix("AI_REVIEWER")`,
`WithDisallowUnknownFields()`, `WithSkipFlags()`, YAML-loader, плагины `validate` (go-playground)
и `xconfigvault`. Оставляем осознанное решение текущего кода не использовать tag-дефолты для
булевых флагов там, где явный `false` должен пережить дефолт (`WithSkipDefaults` там, где это важно).

`teams` — slice-of-struct, xconfig умеет разворачивать её из env
(`AI_REVIEWER_TEAMS_0_NAME`, `AI_REVIEWER_TEAMS_0_REPOSITORIES`), что позволяет конфигурировать
команды целиком через env в k8s.

### 7.3 Fail-fast валидация

Часть — тегами `validate` (`required`, `url`, `oneof`, `dive`), часть — доменной проверкой
(ошибки агрегируются и печатаются списком):

1. `teams` не пуст; 2. имена уникальны (case-insensitive); 3. у каждой команды непустой
   `repositories`; 4. валидный `slack_channel`; 5. каждый repository — валидный GitLab path либо
   числовой id; 6. один repository не в двух командах (иначе ошибка с обоими именами);
2. `gitlab.base_url` — абсолютный http(s) URL; 8. `gitlab.token` непустой;
3. `llm.claude.auth.mode` ∈ {existing-login, oauth-token, api-key} и соответствующий секрет
   непуст (для `existing-login` секреты не требуются, а заданные — ошибка конфигурации);
4. `slack.token` непустой, если `slack_send_enabled` или заданы каналы; 11. `scan_interval ≥ 1m`,
   `max_parallel ≥ 1`, `max_comments ≥ 1`; 12. существующие проверки pipeline-режимов и verifier'ов;
5. `time.LoadLocation("Europe/Moscow")` успешна; 14. Postgres DSN собирается и коннект проходит.

---

## 8. Domain-слой

```go
// internal/domain — чистый пакет без I/O

type Team struct {
    Name         string
    SlackChannel string
    AIReview     bool
    Repositories []string
}

type MergeRequestSnapshot struct {
    Team         string
    Project      Project
    MR           MergeRequest
    HeadSHA      string
    Reviewers    []Reviewer      // User + ReviewState (из GraphQL)
    ApprovedBy   []User
    Discussions  []Discussion
    Mergeability Mergeability    // HasConflicts, DetailedStatus, Known bool
    Pipeline     Pipeline        // head_pipeline: ID, SHA, Status, WebURL, Known bool
    LastPushAt   time.Time       // created_at последней diff-версии
}
```

Классификаторы (чистые функции, таблично-тестируемые):

```go
func NeedsAIReview(s Snapshot, teamEnabled bool, lastReviewedSHA string) (bool, Reason)
func NeedsHumanReview(s Snapshot, r Reviewer) bool
func UnresolvedThreads(s Snapshot) int
func HasMergeConflicts(s Snapshot) (conflict bool, known bool)
func FailedPipeline(s Snapshot) (p Pipeline, failed bool, known bool)
```

**Unresolved threads** — только резолвабельные незакрытые: дискуссия учитывается, если
`individual_note == false`, `notes[0].resolvable == true` и ни одна заметка треда не
`resolved == true`. Обычные комментарии не считаются.

**Merge conflicts** — только из полей мержабельности: `has_conflicts == true`
(с подтверждением `detailed_merge_status`). Если мержабельность ещё не вычислена
(`merge_status` = `unchecked`/`checking`) — «неизвестно», конфликт не рапортуется.
Никакого вывода конфликта из текста pipeline или комментариев.

**Failed pipeline** — упавший пайплайн головного коммита. Источник — `head_pipeline`
из детального ответа MR (он уже загружен, дополнительных запросов не нужно). Правила:

- «упал» ⇔ `status == "failed"`. Значения `canceled`/`canceling`/`skipped`/`manual` падением
  **не считаются** — это не сигнал «автору надо чинить»; `running`/`pending`/`created`/
  `preparing`/`waiting_for_*`/`scheduled` — ещё не результат;
- пайплайн учитывается только если `head_pipeline.sha == s.HeadSHA`. Пайплайн от прошлого пуша
  — устаревший: `known = false`, в дайджест не попадает (иначе автору показывали бы падение,
  которое он уже починил);
- `head_pipeline` в GitLab «exposed only if the current user can view pipelines for this project»
  — если поля нет, `known = false`. Одноразовый (на прогон) фолбэк на
  `GET …/merge_requests/:iid/pipelines` берётся только для тех MR, где поле отсутствует,
  а не для всех;
- в дайджест выводится ссылка `head_pipeline.web_url`, чтобы автор попадал сразу в упавший
  пайплайн, а не в MR.

Это чистая функция над снапшотом: никакого парсинга статуса из текста комментариев.

**Human review** — на основе `ReviewState` из GraphQL: ревьюеру нужно действие, если его состояние
не `REVIEWED`/`APPROVED` (либо он `REQUESTED_CHANGES` и после этого был новый пуш). Fallback,
если GraphQL недоступен (`graphql_enabled: false` или ошибка): ревьюер не в `approved_by`
и не оставил не-system заметку после `LastPushAt`.

---

## 9. GitLab API

### 9.1 REST v4

| Операция                      | Endpoint                                                             | Когда                                                                    |
| ----------------------------- | -------------------------------------------------------------------- | ------------------------------------------------------------------------ |
| Проверка проекта / метаданные | `GET /projects/:key`                                                 | старт (валидация), первое касание                                        |
| Открытые MR репозитория       | `GET /projects/:key/merge_requests?state=opened&per_page=100`        | каждый scan/digest                                                       |
| Детали MR (+ `head_pipeline`) | `GET /projects/:key/merge_requests/:iid`                             | кандидат прошёл дешёвый отсев                                            |
| Изменённые файлы              | `GET /projects/:key/merge_requests/:iid/diffs`                       | только перед ревью                                                       |
| Версии диффа (время пуша)     | `GET /projects/:key/merge_requests/:iid/versions`                    | digest + interdiff                                                       |
| Коммиты MR                    | `GET /projects/:key/merge_requests/:iid/commits`                     | только перед ревью                                                       |
| Дискуссии                     | `GET /projects/:key/merge_requests/:iid/discussions`                 | всегда (треды + маркеры + контекст)                                      |
| Approvals                     | `GET /projects/:key/merge_requests/:iid/approvals`                   | digest (доступно на Free)                                                |
| Pipelines                     | `GET /projects/:key/merge_requests/:iid/pipelines`                   | контекст ревью; в digest — только фолбэк, когда `head_pipeline` не отдан |
| Сырой файл                    | `GET /projects/:key/repository/files/:path/raw?ref=`                 | контекст (fallback без worktree)                                         |
| Обзорная заметка              | `POST /projects/:key/merge_requests/:iid/notes`                      | summary + маркер                                                         |
| Inline-тред                   | `POST /projects/:key/merge_requests/:iid/discussions` (+ `position`) | публикация findings                                                      |
| Проверка токена               | `GET /user`                                                          | doctor                                                                   |

Draft notes API удаляется — automation-first публикует сразу.

### 9.2 GraphQL (новое)

REST v4 **не отдаёт review state ревьюера**: `reviewers[].state` — это состояние аккаунта
(`active`/`blocked`). Точное состояние доступно только через GraphQL, поэтому добавляем
минимальный GraphQL-клиент (`POST /api/graphql`, тот же токен, тот же retry):

```graphql
query ($fullPath: ID!, $iids: [String!]) {
  project(fullPath: $fullPath) {
    mergeRequests(iids: $iids, state: opened) {
      nodes {
        iid
        reviewers {
          nodes {
            username
            mergeRequestInteraction {
              reviewState
              approved
              reviewed
            }
          }
        }
      }
    }
  }
}
```

Запрос батчевый (список iid одного проекта) — один POST на репозиторий за прогон.
`graphql_enabled: false` или ошибка/недоступное поле → автоматический фолбэк на REST-эвристику
(§8), с предупреждением в лог один раз за прогон. Это оставляет сервис работоспособным на
инстансах GitLab, где поле отсутствует.

### 9.3 Транспорт

Уже реализовано и сохраняется: retry с backoff через `xutils/retry` (429/5xx/сеть — ретрай,
4xx — `retry.ErrExit`), `context` во всех запросах, `http.Client.Timeout`, кастомный CA.

Добавить: уважение `Retry-After` для 429, метрики `gitlab_requests_total` /
`gitlab_request_errors_total` в транспортном слое, bounded concurrency на уровне сканера.

### 9.4 Эффективность запросов

**Один MR грузится один раз за прогон**: внутри `scan`/`digest` живёт
`snapshotCache map[projectID_iid]*Snapshot`. Дискуссии — один запрос, из него берутся и
unresolved-треды, и review-маркер, и fingerprints, и контекст промпта. Тяжёлые вызовы
(diffs, commits, worktree) — только после того, как дешёвая классификация сказала «нужно ревью».

---

## 10. Состояние ревью: БД + маркеры в GitLab

### 10.1 Два уровня

**Первичный** — Postgres: `mr_reviews` (успешный SHA) и `mr_findings` (опубликованные fingerprints).
Это то, на что смотрит планировщик.

**Вторичный** — маркеры в GitLab. Они почти бесплатны (пишутся в заметку, которую мы и так
публикуем; читаются из дискуссий, которые и так загружены) и дают три вещи:
пересоздание/потеря БД не приводит к повторной публикации findings; человек видит,
что ревью было; при ручном разборе понятно, какой комментарий чей.

### 10.2 Формат маркера ревью

Заметка публикуется **последней**, после всех findings:

```markdown
🤖 **AI review** — 3 finding(s), risk: medium

<!-- ai-reviewer:review:v1 {"v":1,"project_id":123,"mr_iid":456,"head_sha":"abcdef0123…","reviewed_at":"2026-08-13T09:12:44Z","findings":3,"pipeline":"standard","tool":"1.4.0"} -->
```

Детерминирован, machine-readable, версионирован (`:v1` + `"v":1`; незнакомые версии игнорируются
без падения), безопасен (никаких токенов), в rendered-виде GitLab не показывается.

### 10.3 Маркер finding

```markdown
**[high/correctness] Возможная утечка соединения**
… тело …

<!-- ai-reviewer:finding:v1 fp=9f2c1ab34de55701 -->
```

`fp` — существующий `review.Fingerprint(projectID, mrIID, filePath, category, title)`.
Он **не зависит от head SHA**, поэтому один и тот же finding не спамится после каждого пуша.
Перед ревью строится `ExistingFingerprints` = (fingerprints из `mr_findings`) ∪ (fp-маркеры
из дискуссий) и передаётся в движок — механизм уже существует и покрыт тестом
(`validator_test.go`), меняется только источник.

### 10.4 Порядок и atomic success

```text
1. LLM pipeline отработал без ошибки
2. детерминированная валидация выполнена
3. INSERT mr_reviews (status='running'|'dry_run') + findings
4. если publish включён — публикация findings, каждый со своим fp-маркером,
   note_id/published_at пишутся сразу после успешного POST
5. ПОСЛЕДНЕЙ — summary-заметка с review-маркером
6. UPDATE mr_reviews SET status='succeeded'
```

Крэш между 4 и 6 → нет ни маркера, ни `succeeded` → следующий `scan` поставит джобу снова,
но уже опубликованные findings отсеются по `mr_findings`/fp-маркерам. Ложного «успеха» не бывает:
`succeeded` ставится последним. Упавшее ревью пишет `status='failed'` + `error`, инкрементит
`ai_reviews_failed_total`, не блокирует остальные MR и повторяется на следующем скане.

**Dry-run** (`ai_review_publish_enabled: false`): пункты 4–5 пропускаются, но
`mr_reviews(status='dry_run')` пишется — поэтому повторного ревью того же SHA каждые 5 минут
не происходит. Именно БД закрывает эту дыру.

---

## 11. Что меняется в review-движке

| Вход движка              | Было (SQLite)            | Стало                                                        |
| ------------------------ | ------------------------ | ------------------------------------------------------------ |
| `ExistingFingerprints`   | `db.ListFindingsByMR`    | `mr_findings` ∪ fp-маркеры из дискуссий GitLab               |
| `PriorReview`            | Прошлые записи `reviews` | `mr_reviews`/`mr_findings` + наши треды и ответы людей в них |
| interdiff                | `prev.head_sha` из БД    | `mr_reviews.head_sha` → `git diff prev..head` в worktree     |
| `RelatedFiles`           | FTS5 (`internal/index`)  | **удаляется**; в agent mode Claude ищет сам (Grep/Glob)      |
| `Memory` (review memory) | таблица `review_memory`  | **удаляется** (personal-фича «запомнить контекст из UI»)     |
| Персист результата       | `reviews`/`findings`     | `mr_reviews`/`mr_findings` + публикация в GitLab + метрики   |

Новое полезное свойство: **реакции людей на наши треды становятся входом ревью** — если тред уже
resolved или в нём есть ответ разработчика, это видно в снапшоте дискуссий и передаётся в промпт
как «тема обсуждена».

Инварианты движка не трогаем: Go владеет валидацией и позициями, findings только на изменённых
строках, severity threshold, cap, scrubbing, binary/vendored/generated не доходят до LLM.

---

## 12. Claude Code CLI

### 12.1 Интерфейс

`llm.Client` сохраняется (`Review`, `CompleteJSON`, `Ask`), реализация — `ClaudeCLI`,
вызов `claude -p --output-format json --json-schema …`. Флаги остаются конфигурируемыми
(`bin`, `model`, `permission_mode`, `allowed_tools`, `extra_args`).

Проверено эмпирически на `claude 2.1.222`: актуальны `-p`, `--output-format json`, `--json-schema`,
`--append-system-prompt`, `--permission-mode`, `--allowedTools`, `--model`.
Существующий комментарий про **`--bare`** остаётся верным: `--bare` пропускает keychain и жёстко
требует `ANTHROPIC_API_KEY`, ломая `existing-login` — не используем.

### 12.2 Детерминированная аутентификация

Отдельный тестируемый компонент `internal/llm/auth.go`:

```go
type ClaudeAuth interface {
    Env(base []string) ([]string, error)   // окружение subprocess
    Describe() string                      // безопасное для логов описание, без значений
}
```

| mode             | `ANTHROPIC_API_KEY`                          | `CLAUDE_CODE_OAUTH_TOKEN`                    | Прочее                                |
| ---------------- | -------------------------------------------- | -------------------------------------------- | ------------------------------------- |
| `existing-login` | **удаляется** из унаследованного окружения   | **удаляется**                                | используется логин среды              |
| `oauth-token`    | **принудительно удаляется**                  | значение из конфига/Vault, ошибка если пусто | parent API-key не может «выиграть»    |
| `api-key`        | значение из конфига/Vault, ошибка если пусто | **принудительно удаляется**                  | подписочный токен не может «выиграть» |

Во всех режимах удаляются конфликтующие провайдерские переменные (`CLAUDE_CODE_USE_BEDROCK`,
`CLAUDE_CODE_USE_VERTEX`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_BASE_URL`), если оператор не включил
их явно через `llm.claude.extra_env` — иначе «сюрприз из окружения ноды» незаметно меняет
биллинг и модель.

Секреты регистрируются в `security.RegisterSecret` до первого запуска subprocess.

`oauth-token` получается оператором один раз: `claude setup-token` (требует подписку),
результат кладётся в Vault/Secret. В YAML — только пустое поле-плейсхолдер.

### 12.3 Agent mode

```text
MR → ephemeral mirror/fetch → worktree на head SHA (detached) → cwd для claude → ревью → worktree удаляется
```

Уже реализовано в `internal/git`: аутентификация через `http.extraHeader` в env (git 2.31+
`GIT_CONFIG_*`), токен не попадает ни в URL клона, ни в конфиг зеркала, ни в argv.
Корень кэша переносится в `review.workdir` (по умолчанию `/work`).

Права Claude: `permission_mode: dontAsk` + узкий `allowed_tools` (`Read`, `Grep`, `Glob`,
`Bash(git diff *)`, `Bash(git log *)`, `Bash(git show *)`). Никаких `Edit`/`Write`/`git push`.
Публикация в GitLab — исключительно Go-кодом после валидации.

### 12.4 Диагностика

`claude auth status --json` **существует и проверен** (поля `loggedIn`, `authMethod`, `apiProvider`,
`subscriptionType`, `orgName`, `email`). Doctor использует его вместо парсинга человекочитаемого
текста; печатает только `loggedIn`/`authMethod`/`subscriptionType` — email и orgId не выводит.

---

## 13. Slack

### 13.1 Клиент

Минимальный клиент в стиле GitLab-клиента (без новой зависимости): `users.list` (cursor-пагинация,
`limit=200`), `chat.postMessage`, `auth.test`, `conversations.info` (для doctor).
Slack возвращает ошибки с HTTP 200 и `{"ok":false,"error":"..."}` — клиент это учитывает;
`ratelimited` ретраится с уважением `Retry-After` (`users.list` — Tier 2, ~20 запросов/минуту).

**Scopes бота:** `users:read`, `users:read.email` (иначе email в `users.list` недоступен),
`chat:write` (бот должен быть в каналах).

### 13.2 Matcher

```go
type UserMatcher interface { Match(ctx context.Context, u gitlab.User) (Result, error) }
type Result struct { Status Status; SlackID, Display string } // Matched | Ambiguous | NotFound
```

Порядок:

1. **Явный override** из `slack.user_map` (`gitlab_username → SLACK_USER_ID`) — escape-hatch;
2. **Кэш** `slack_user_links` в Postgres (переживает рестарт, обновляется по TTL);
3. **Email** — нормализованный (trim + lowercase) email GitLab-пользователя против Slack-индекса;
4. **Username/имя** — GitLab `username` против Slack `name`/`display_name`/`real_name`,
   затем GitLab `name` против `real_name`/`display_name`; сравнение регистронезависимое,
   с trim и схлопыванием пробелов, без агрессивного fuzzy;
5. **Ambiguous** (>1 кандидата) — случайный выбор не делается: `Ambiguous` + warning + метрика,
   в дайджесте выводится текстом;
6. **Not found** — `John Smith (@john)` без mention; дайджест всё равно отправляется.

Внутри прогона — один `users.list`, из него строятся индексы `email→user`,
`display_name→[]user`, `real_name→[]user`, `handle→[]user`.

**Ограничение**: GitLab REST отдаёт чужой `email` только админскому токену; у обычного
service-токена доступен лишь `public_email`, часто пустой. Поэтому шаг 3 сработает не всегда —
ради этого и добавлены `user_map` и кэш.

### 13.3 Block Kit и лимиты

```text
📋 MR Digest — Payments

👀 Reviews needed
<@U123> — 2 MRs
• payments !481 — Add payment retries
  by <@U456> · waiting 18h
• billing !932 — Invoice export
  by <@U789> · waiting 5h

🛠 Author actions
<@U456>
• payments !475 — Cache invalidation
  💬 3 unresolved threads
  ⚠️ merge conflicts
  ❌ pipeline failed
<@U789>
• checkout !122 — Search filters
  ❌ pipeline failed
```

MR попадает в «Author actions», если у него есть хотя бы одно из трёх: unresolved threads,
merge conflicts, упавший пайплайн. Порядок строк внутри MR фиксирован
(threads → conflicts → pipeline), чтобы дайджест читался одинаково изо дня в день.
`❌ pipeline failed` — кликабельная ссылка на `head_pipeline.web_url`.

Все MR — кликабельные ссылки (`web_url`). Лимиты соблюдаются билдером: ≤50 блоков на сообщение,
≤3000 символов в `text` секции. При превышении — разбиение на пронумерованные части
(`MR Digest — Payments (1/3)`), **без молчаливого усечения**.

### 13.4 Dry-run и отсутствие шума

`slack_send_enabled: false`: полностью выполняются сканирование, классификация, матчинг и сборка
Block Kit payload; не вызывается только `chat.postMessage`, payload логируется как preview,
в `digest_runs` пишется `status='dry_run'`.

Сканер **не пишет в Slack** по каждому MR: Slack — слой уведомлений о действиях людей,
findings живут в GitLab.

---

## 14. Lifecycle, логи, метрики (`/mx`)

### 14.1 Launcher

```go
ln := launcher.New(
    launcher.WithName("ai-reviewer"),
    launcher.WithVersion(version.String()),
    launcher.WithLogger(l),
    launcher.WithAppStartStopLog(true),
    launcher.WithGlobalShutdownTimeout(90*time.Second),
    launcher.WithRunnerServicesSequence(launcher.RunnerServicesSequenceLifo),
    launcher.WithOpsConfig(cfg.Ops),
)
ln.ServicesRunner().Register(
    launcher.NewService(launcher.WithService(pgService), launcher.WithStartupPriority(1)),
    launcher.NewService(launcher.WithService(jobsService)),   // River client
)
return ln.Run()
```

Postgres поднимается в приоритетной группе 1 (сообщает readiness), River — после.
Graceful shutdown: SIGTERM → отмена root-контекста → River перестаёт брать новые джобы и
дренирует текущие в пределах `jobs.drain_timeout` → незавершённые джобы остаются durable
и переподхватятся → чистятся worktree'ы → ops HTTP гасится лаунчером. Второй сигнал — форс-выход.

### 14.2 Логи: полная миграция на `mx/logger` (zap)

`log/slog` уходит из всех пакетов; интерфейс — `logger.Logger` (`logger.ExtendedLogger` в `main`).
Конфигурация — `logger.Config` в `config.yaml` (`format: json`, `level`).

Редакция секретов переносится с slog-хендлера на **обёртку `zapcore.Core`**:

```go
// internal/security/zapcore.go
func NewRedactingCore(inner zapcore.Core) zapcore.Core  // маскирует message и строковые поля
// подключение: logger.NewExtended(..., logger.WithZapOption(zap.WrapCore(security.NewRedactingCore)))
```

Плюс второй рубеж — тип `config.Secret`, у которого `String()`/`MarshalJSON`/`MarshalYAML`
возвращают `[redacted]`, поэтому случайный `%v` секрета невозможен.
Явный `security.Mask()` для вывода subprocess и текста ошибок остаётся как есть.

Структурные поля: `operation`, `team`, `repository`, `project_id`, `mr_iid`, `head_sha` (8 символов),
`duration`, `result`, `trigger` (`schedule|manual|cli`), `job_id`.

### 14.3 Метрики

mx ops отдаёт `promhttp.Handler()`, т.е. **дефолтный prometheus registry** (проверено в
`launcher/ops/metrics.go@v0.6.0`). Свой стек не заводим — регистрируем через `promauto`:

```text
ai_reviewer_scans_total{result}
ai_reviewer_scan_duration_seconds              (histogram)
ai_reviews_total{team}
ai_reviews_failed_total{team,reason}
ai_reviews_skipped_total{team,reason}          (up_to_date|draft|disabled)
ai_review_duration_seconds{team}               (histogram)
ai_review_cost_usd_total{team}
gitlab_requests_total{endpoint,method}
gitlab_request_errors_total{endpoint,status}
merge_requests_scanned_total{team}
merge_requests_waiting_human_review_total{team}    (gauge)
merge_requests_with_unresolved_threads_total{team} (gauge)
merge_requests_with_conflicts_total{team}          (gauge)
merge_requests_with_failed_pipeline_total{team}    (gauge)
slack_digest_runs_total{team,result}
slack_messages_sent_total{team}
slack_send_errors_total{team}
slack_user_match_total{result}                  (matched|not_found|ambiguous)
river_jobs_total{kind,state}                    (из журнала River)
```

### 14.4 Health / readiness

- `/livez` — процесс жив, без сетевых вызовов;
- `/readyz` — конфиг загружен, пул Postgres живой (`Ping`), River-клиент запущен, TZ загружена.
  **Никакого обхода репозиториев GitLab на каждый запрос.**
- Глубокая проверка GitLab/Slack/Claude — задача `doctor`, не probe.

---

## 15. CLI

```bash
ai-reviewer serve                       # production: mx lifecycle, ops, River (scan + digest)
ai-reviewer scan   [--team <name>]      # one-shot: поставить джобы ревью по конфигурации
ai-reviewer digest [--team <name>]      # one-shot Slack digest
ai-reviewer review <ref> [--publish]    # ручное ревью одного MR тем же пайплайном
ai-reviewer doctor                      # диагностика
ai-reviewer migrations up|down|create   # миграции (app + River)
```

`<ref>` сохраняет форматы `internal/gitlab/ref.go` (тесты остаются): полный URL MR,
`group/subgroup/repo!123`, `project-id:iid`.

`review` использует **тот же** pipeline и тот же publisher, что и джоба — второй реализации нет
(CLI-путь выполняет ту же функцию воркера синхронно). По умолчанию действует
`service.ai_review_publish_enabled`; `--publish`/`--no-publish` переопределяют для конкретного запуска.

Удаляются: `sync`, `daemon`, алиас `start`, `--auto-review/--auto-draft/--auto-publish`,
`--open`, `--foreground`, проверки SQLite/FTS5 в doctor.

**Новый doctor**: конфиг (все правила §7.3); Postgres (коннект + применённость миграций +
таблицы River); GitLab connectivity/auth (`GET /user`) и доступность GraphQL; доступность каждого
сконфигурированного repository (`GET /projects/:key`, параллельно, с указанием нерезолвнутых);
видимость пайплайнов (приходит ли `head_pipeline` — иначе строка «pipeline failed» в дайджесте
не появится);
`claude` в PATH, `claude --version`, `claude auth status --json` против выбранного `auth.mode`;
Slack `auth.test` + членство бота в каждом канале; таймзона `Europe/Moscow`; наличие `git`;
доступность `review.workdir` на запись. Значения секретов не печатаются.

---

## 16. Docker / Kubernetes

### 16.1 Dockerfile

```dockerfile
FROM golang:1.26-alpine AS build
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X …/internal/version.Version=$VERSION" \
    -o /out/ai-reviewer ./cmd/ai-reviewer

FROM alpine:3.22
RUN apk add --no-cache ca-certificates git bash curl libgcc libstdc++ ripgrep tzdata \
 && wget -O /etc/apk/keys/claude-code.rsa.pub https://downloads.claude.ai/keys/claude-code.rsa.pub \
 && echo "https://downloads.claude.ai/claude-code/apk/stable" >> /etc/apk/repositories \
 && apk update && apk add --no-cache claude-code \
 && adduser -D -u 10001 app && mkdir -p /work && chown app:app /work
ENV TZ=Europe/Moscow HOME=/home/app USE_BUILTIN_RIPGREP=0 DISABLE_AUTOUPDATER=1
COPY --from=build /out/ai-reviewer /usr/local/bin/ai-reviewer
USER app
WORKDIR /work
ENTRYPOINT ["ai-reviewer"]
CMD ["serve"]
```

Детали по актуальной документации Claude Code: на musl нужны `bash`, `curl`, `libgcc`,
`libstdc++`, `ripgrep` + `USE_BUILTIN_RIPGREP=0`; apk-репозиторий Claude Code подписан
(ключ проверяется); альтернативы — `curl -fsSL https://claude.ai/install.sh | bash` или
`npm i -g @anthropic-ai/claude-code` (Node 22+, ставит тот же нативный бинарь).
Автообновление в неизменяемом контейнере отключено — версия фиксируется образом.
Никаких credentials в образе. `/work` — ephemeral writable каталог.

### 16.2 Секреты и деплой

```bash
docker run --rm \
  -e AI_REVIEWER_GITLAB_TOKEN=... \
  -e AI_REVIEWER_SLACK_TOKEN=... \
  -e AI_REVIEWER_LLM_CLAUDE_AUTH_OAUTH_TOKEN=... \   # либо ..._API_KEY
  -e AI_REVIEWER_POSTGRES_HOST=... -e AI_REVIEWER_POSTGRES_PASSWORD=... \
  -v $PWD/config.yaml:/etc/ai-reviewer/config.yaml:ro \
  ai-reviewer:latest serve --config /etc/ai-reviewer/config.yaml
```

Либо через Vault: `VAULT_ENABLED=true`, `VAULT_ADDR`, `VAULT_SECRET_PATH`,
`VAULT_AUTH_KIND=kubernetes`, `VAULT_KUBE_ROLE` — секреты подтягиваются плагином при старте
и обновляются фоном (`VAULT_REFRESH_INTERVAL`).

Kubernetes: `Deployment` (реплики ≥1, конкуренцию снимает River), `ConfigMap` для `config.yaml`,
`Secret`/Vault для токенов, **init-контейнер** `ai-reviewer migrations up`,
probes `/livez` и `/readyz` на ops-порту, `emptyDir` на `/work`,
`terminationGracePeriodSeconds` > `jobs.drain_timeout`, requests/limits с запасом по памяти
(Claude Code требует 4 GB+ RAM).

`existing-login` внутри immutable-контейнера не является основным сценарием: интерактивный логин
требует браузера и записи в `~/.claude`, теряющейся при рестарте. Для контейнера — `oauth-token`
или `api-key`.

---

## 17. Тесты

Без реального GitLab/Slack/Claude: GitLab (REST и GraphQL) и Slack — через `httptest.Server`;
Claude CLI — через fake-executable (`os.Args[0]` + `TestHelperProcess`, как в существующем
`internal/llm/claude_cli_test.go`). Postgres-зависимые тесты — опциональные, пропускаются при
отсутствии `AI_REVIEWER_TEST_PG_DSN` (`make dev` поднимает Postgres в docker-compose),
`make test` без БД остаётся зелёным.

### Новые обязательные тесты

**Состояние ревью**

- нет записи `mr_reviews` → ревью требуется; запись с текущим SHA → skip; со старым SHA → re-review;
- упавшее ревью → `status='failed'`, следующий скан повторяет;
- dry-run пишет `status='dry_run'` и не приводит к повторному ревью;
- порядок публикации: summary-маркер последним (проверка последовательности вызовов fake-API);
- маркеры: парсинг v1, несколько маркеров → берётся свежий, незнакомая версия → игнор без паники;
- finding с существующим fp (в БД или в маркере) не публикуется повторно.

**Teams**

- несколько команд; repository попадает в свою; дубликат в двух командах → ошибка валидации
  с обоими именами; данные команд не смешиваются в дайджесте.

**Human review**

- нужен ревью; уже отревьюено/approved; Draft игнорируется; merged/closed игнорируются;
- re-review после нового пуша; GraphQL-состояния (`UNREVIEWED`/`REVIEWED`/`REQUESTED_CHANGES`);
- фолбэк на REST-эвристику, когда GraphQL отключён/сломан.

**Discussions / conflicts**

- unresolved+resolvable считается; resolved не считается; individual note не считается;
  несколько тредов частично закрыты;
- `has_conflicts=true` → конфликт; `false` → нет; `unchecked/checking` → «неизвестно».

**Failed pipeline**

- `status=failed` и `sha == head_sha` → в дайджест, со ссылкой `web_url`;
- `status=failed`, но `sha` от прошлого пуша → устаревший, в дайджест не попадает;
- `success`/`running`/`pending`/`created`/`manual`/`scheduled` → не падение;
- `canceled`/`canceling`/`skipped` → не падение (отдельный кейс, легко перепутать с failed);
- `head_pipeline` отсутствует (нет прав на просмотр пайплайнов) → `known=false`,
  фолбэк на `/pipelines` вызывается ровно для таких MR и ровно один раз;
- MR без тредов и конфликтов, но с упавшим пайплайном → всё равно попадает в «Author actions».

**Slack matcher**

- точный email; нормализация регистра; fallback username; fallback имя; ambiguous (без случайного
  выбора); not found (дайджест всё равно уходит); override `user_map` побеждает; кэш исключает
  повторный HTTP-запрос.

**Флаги dry-run**

- `slack_send_enabled=false`: дайджест собран полностью, `chat.postMessage` не вызван;
- `ai_review_publish_enabled=false`: ревью выполнено, валидация пройдена, ни один GitLab-write не вызван.

**Claude auth**

- `existing-login`: auth-переменные не подставляются, унаследованные удалены;
- `oauth-token`: OAuth присутствует, унаследованный `ANTHROPIC_API_KEY` удалён;
- `api-key`: ключ присутствует, унаследованный OAuth удалён;
- значения токенов не появляются ни в логах (через redacting-core), ни в тексте ошибок;
- пустой обязательный секрет → внятная ошибка на старте.

**Scheduler**

- `Daily.Next` даёт ровно 09:00 и 16:30 Europe/Moscow при `TZ=UTC` и `TZ=Asia/Tokyo`;
- переход через полночь и через сутки; корректная работа как `river.PeriodicSchedule`.

**Конкуренция (River)**

- две вставки `ReviewArgs` с одинаковым `(project, iid, sha)` → одна джоба (unique);
- повторная вставка после завершения предыдущей разрешена;
- `digest_runs` уникальность `(team, run_date, slot)` не даёт задвоить дайджест.

**Partial failures**

- 10 репозиториев, один отдаёт 500 → остальные 9 обработаны, дайджест помечен
  `⚠️ Partial data: failed to inspect 1 repository.`;
- классификация ошибок: fatal (401/403/конфиг) vs retryable (429/5xx) vs partial.

**Block Kit**

- разбиение большого дайджеста на пронумерованные части, ничего не теряется молча;
- лимиты 50 блоков / 3000 символов соблюдены.

Сохраняемые тесты движка адаптируются по сигнатурам, поведение не меняется. Прогон — `go test -race ./...`.

---

## 18. Документация

**README** переписывается полностью. Позиционинг:

> AI Reviewer is a self-hosted GitLab review agent for engineering teams.
> It automatically reviews merge requests with Claude Code, tracks human review and author actions,
> and sends actionable Slack digests.

Разделы: overview; architecture; team model; configuration (YAML/env/Vault); PostgreSQL и миграции;
GitLab permissions (PAT service account, scope `api`, роль не ниже Reporter — в том числе ради
видимости `head_pipeline`); Slack permissions
(`users:read`, `users:read.email`, `chat:write`); Claude auth modes; subscription token setup
(`claude setup-token` → Vault/Secret); API key setup; Docker; Kubernetes (реплики, init-контейнер
миграций, probes, emptyDir); dry-run modes; AI review scheduler; Slack digest scheduler;
Europe/Moscow; модель состояния (Postgres + GitLab-маркеры); user matching; security model;
troubleshooting.

Обязательная заметка: подписочные OAuth-креденшелы предназначены для использования Claude Code
владельцем соответствующей подписки; `ai-reviewer` не собирает и не проксирует подписочные
креденшелы других пользователей; для организационного/коммерческого развёртывания следует
свериться с актуальными условиями Anthropic и при необходимости использовать
API key / Team / Enterprise / cloud-provider аутентификацию.

**CLAUDE.md** переписывается как authoritative developer guide новой архитектуры.

Удаляемые инварианты: ручное одобрение перед публикацией; personal drafts; SQLite как источник
правды; `reviews_for_me`.

Сохраняемые: Go владеет валидацией findings и позициями GitLab; Claude никогда не пишет в GitLab
напрямую; никогда не approve/merge; никаких секретов в логах; findings только на изменённых строках;
дедупликация findings; bounded concurrency; binary/vendored/generated не доходят до LLM.

Новые: GitLab — источник правды о MR, Postgres — операционное состояние сервиса;
изоляция команд; детерминированный Claude auth; success-маркер пишется последним;
Slack — слой уведомлений, а не хранилище состояния; конкуренцию реплик снимает River
(unique jobs + leader election), ручных локов не пишем.

Существующие аналитические документы (`docs/competitive-analysis-*.md`, `docs/gopls-mcp-analysis.md`)
сохраняются как есть — это история анализа, а не описание архитектуры.

---

## 19. Этапы работ

| Этап | Содержание                                                                                                                                                | Готовность подтверждается                        |
| ---- | --------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------ |
| 0    | Зависимости: `+tkcrm/mx`, `+jackc/pgx/v5`, `+riverqueue/river(+riverpgxv5,rivertype)`, `+xconfigvault`, `+go-playground/validator`, `−modernc.org/sqlite` | `go mod tidy`, сборка                            |
| 1    | `internal/config`: новая схема (teams/slack/service/postgres/jobs/ops/log), `Secret`, Vault, валидация; удаление schema/patch/template                    | `config_test.go` зелёный                         |
| 2    | Удаление personal-слоя: `state`, `server`, `ui`, `index`, `skills` + связанные сервисы/тесты                                                              | `go build ./...`                                 |
| 3    | `sql/migrations` + `sql/pgxgen.yaml` + `internal/store`/`internal/models`; команда `migrations`                                                           | `make migrateup` на локальном Postgres           |
| 4    | Логгер: миграция на `mx/logger`, `security.NewRedactingCore`, вычистка `slog`                                                                             | `redaction_test.go` на zap-core                  |
| 5    | `internal/domain` + расширение `internal/gitlab` (approvals, versions, notes/discussions POST, маркеры, GraphQL)                                          | тесты классификаторов, маркеров, GraphQL-фолбэка |
| 6    | `internal/llm/auth.go` (ClaudeAuth) + проброс в ClaudeCLI                                                                                                 | `llm/auth_test.go`                               |
| 7    | `internal/service/review.go`: снапшот → pipeline → publisher → БД/маркер                                                                                  | `service/review_test.go` на fake GitLab          |
| 8    | `internal/slack` + `internal/match` + `internal/service/digest.go`                                                                                        | `httptest`-тесты Slack, matcher                  |
| 9    | `internal/jobs` на River (scan/review/digest/cleanup) + `internal/scheduler/daily.go` + `internal/metrics`                                                | тесты Daily и unique-джоб                        |
| 10   | `internal/app` + mx launcher; `internal/cli`: serve/scan/review/digest/doctor/migrations                                                                  | ручной прогон всех команд                        |
| 11   | Dockerfile, docker-compose (postgres), k8s-пример, README, CLAUDE.md                                                                                      | сборка образа, `claude --version` внутри         |
| 12   | Финальная зачистка: dead code, grep-проверки, `make fmt test lint build`                                                                                  | все команды зелёные                              |

---

## 20. Риски и ограничения

### 20.1 GraphQL на self-managed

Поле `mergeRequestInteraction.reviewState` может отсутствовать на старых версиях GitLab.
Меры: `graphql_enabled` + автоматический фолбэк на REST-эвристику + явная проверка в `doctor`.

### 20.2 Видимость пайплайнов

`head_pipeline` отдаётся только если у токена есть право просматривать пайплайны проекта.
У service account должна быть роль не ниже Reporter (для приватных проектов), иначе строка
`❌ pipeline failed` просто не появится — молча ничего не сломается, но и сигнала не будет.
`doctor` проверяет это явно: для одного MR из каждого репозитория смотрит, приходит ли
`head_pipeline`, и предупреждает, если нет.

### 20.3 «waiting 18h»

Времени назначения ревьюера REST не отдаёт (resource-events для reviewer нет). Считаем время
ожидания от `created_at` последней diff-версии (время последнего пуша) — семантически «сколько
ревьюер не реагировал на текущее состояние MR». Ограничение фиксируется в README.

### 20.4 Исполнение кода чужих репозиториев

В personal-режиме verifier'ы `go_test`/`tsc` и `review.coverage` исполняли твой код на твоей машине.
В team-сервисе это исполнение кода из произвольных репозиториев на общем хосте — эскалация модели
угроз. `go_test`, `tsc`, `coverage` остаются **выключенными по умолчанию** с явным предупреждением
в README; дефолтный набор — `go_build`, `go_vet`, `py_syntax`.

### 20.5 Стоимость

Каждый новый head SHA запускает полный pipeline (в `standard` — 2 прохода + skeptic).
Меры: запись отревьюенного SHA в БД, `review.max_parallel`, `MaxAttempts=1` у джобы ревью,
дешёвый отсев до тяжёлых запросов, метрики `ai_review_cost_usd_total` и `ai_reviews_*`.

### 20.6 Rate limits

GitLab self-managed ограничивает запросы на пользователя; Slack `users.list` — Tier 2.
Меры: один `users.list` на прогон, snapshot-кэш, bounded concurrency, уважение `Retry-After`,
экспоненциальный backoff (уже есть).

### 20.7 Эксплуатационная зависимость

Появляется обязательный PostgreSQL: его недоступность останавливает сервис (readiness fail,
джобы не берутся). Это сознательная цена за корректную работу нескольких реплик.

---

## 21. Definition of done

- [ ] personal mode удалён (нет UI, нет approve/reject/draft/publish-подтверждения, нет `reviews_for_me`)
- [ ] SQLite удалён (`modernc.org/sqlite` отсутствует в `go.mod`, `internal/state` удалён)
- [ ] PostgreSQL + pgxgen: схема, миграции, репозитории; `migrations up/down/create` работают
- [ ] River: `scan`/`review`/`digest`/`cleanup`, unique jobs, periodic на лидере, graceful drain
- [ ] ≥2 реплики: один MR не ревьюится дважды, дайджест уходит один раз (тесты + ручная проверка)
- [ ] team config + группировка репозиториев реализованы и провалидированы fail-fast
- [ ] автоматический re-review при смене head SHA работает; dry-run не зацикливается
- [ ] Claude Code CLI сохранён; `existing-login`, `oauth-token`, `api-key` работают, выбор детерминирован
- [ ] секреты: YAML → env → Vault, тип `Secret`, redaction в zap-логах
- [ ] Slack digest, user matching, unresolved threads, conflicts, **упавшие пайплайны**,
      review requirements реализованы
- [ ] 09:00 / 16:30 Europe/Moscow реализованы и покрыты тестами независимо от TZ машины
- [ ] Slack dry-run и AI publish dry-run реализованы как полноценные прогоны
- [ ] health/readiness/metrics работают, graceful shutdown чистит worktree'ы
- [ ] README и CLAUDE.md переписаны, Docker/K8s деплой описан
- [ ] `make fmt && make test && make lint && make build` зелёные, `go test -race ./...` зелёный
- [ ] `git grep -i sqlite`, `git grep -i personal`, `git grep reviews_for_me`, `git grep -i fts5`
      дают только осознанные упоминания в исторических документах
