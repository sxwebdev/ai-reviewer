# Интеграция Linear в MR digest

> Статус: исправленная модель реализована 17 августа 2026 года. Linear не
> создаёт параллельную очередь задач — он уточняет, кому действительно нужно
> действовать по GitLab MR.

## 1. Цель

Использовать текущий workflow status связанной Linear-задачи вместе с реальными
GitLab approvals и review states, чтобы не просить всех назначенных reviewers
повторно смотреть уже принятый MR.

Сервис остаётся read-only: он не меняет Linear issue, GitLab MR, reviewers или
approvals. Готовый Slack payload сохраняется в PostgreSQL, как и раньше.

## 2. Продуктовые правила

Linear применяется только к секции `review` назначенных в GitLab reviewers.
Секция `yours` продолжает показывать действия автора: requested changes,
unresolved threads, conflicts, failed pipeline и новое напоминание о доске.

| Связь и состояние | GitLab | Результат |
|---|---|---|
| задача не найдена | любое | стандартная GitLab-классификация |
| задача найдена, любой status | нет approvals | стандартная GitLab-классификация; MR нельзя потерять из-за случайного перемещения карточки |
| задача найдена, любой status | есть `REQUESTED_CHANGES` | стандартный незавершённый flow; verdict сильнее approvals и Linear status |
| задача `In Review` | есть ≥1 approval, нет `REQUESTED_CHANGES` | оставшиеся reviewers больше не уведомляются; автор получает `move CHAIN-N forward in Linear` |
| задача не `In Review` | есть ≥1 approval, нет `REQUESTED_CHANGES` | review завершён; оставшиеся reviewers не уведомляются |

«Стандартная GitLab-классификация» означает существующие правила проекта и
только людей из `MR.reviewers`. Сервис не расширяет список до всех участников
команды. Если reviewer уже `APPROVED`/`REVIEWED`, он не уведомляется; если он
`UNREVIEWED`, `UNAPPROVED` или `REVIEW_STARTED`, он остаётся в очереди.

`REQUESTED_CHANGES` остаётся блокером, пока такой review state существует. Его
push-aware поведение сохраняется: до нового push действие у автора, после push —
повторный review у запросившего изменения.

## 3. Связывание MR и Linear issue

Идентификатор вида `CHAIN-184` ищется:

1. В заголовке MR.
2. Если там нет валидного совпадения — в имени source branch.

Сравнение регистронезависимое: `chain-184`, `CHAIN-184` и `ChAiN-184`
эквивалентны. Регулярное выражение только извлекает кандидатов. Кандидат
считается валидным лишь после ответа Linear и только если issue принадлежит
одному из `teams[].linear_team_ids`.

Это позволяет проигнорировать ключ другого трекера в заголовке и использовать
валидный Linear identifier из ветки. Если заголовок и ветка содержат разные
валидные Linear issues, побеждает первое совпадение в заголовке, а сервис пишет
warning с выбранным и проигнорированным identifiers.

## 4. Что отображается в Slack

Отдельный плоский список Linear issues удалён. Digest показывает только сводку:

```text
Linear · In Review: 8
```

Это количество всех неархивных `In Review` issues настроенных Linear teams,
включая задачи без открытого MR.

Если связанный MR уже имеет approval, но задача всё ещё `In Review`, в строке
автора появляется действие со ссылкой:

```text
➡️ move CHAIN-184 forward in Linear
```

## 5. Конфигурация

```yaml
linear:
  api_key: "" # AI_REVIEWER_LINEAR_API_KEY; production — env или Vault
  endpoint: https://api.linear.app/graphql
  timeout: 15s
  max_attempts: 4
  max_retry_after: 60s

teams:
  - name: blockchain-api
    slack_channel: C012345678
    linear_team_ids:
      - 2057fa99-d3e7-4610-bc46-48c5399358d6
    repositories: [group/blockchain-api]
```

UUID копируется в Linear UI через `Cmd/Ctrl+K` → `Copy model UUID`. Один UUID
может принадлежать только одной команде приложения. API key требуется только
если хотя бы у одной команды есть `linear_team_ids`.

## 6. Linear API

На сборку одной команды выполняются два вида пагинируемых read-only запросов:

1. `issues` с фильтром `team.id in (…)` и
   `state.name eqIgnoreCase "In Review"` — полный счётчик доски.
2. Один batch lookup кандидатов из открытых MR: `team.id in (…)` и
   `number in (…)`. Номер issue уникален внутри команды, поэтому после ответа
   сервис сопоставляет полный `identifier` и повторно проверяет team UUID.

Batch lookup исключает N+1 запрос на каждый MR и не требует загружать всю
историю Linear workspace. Оба запроса используют cursor pagination, дедупликацию
по UUID и fail-closed проверку team/status на границе клиента.

Клиент всегда проверяет GraphQL `errors`, включая HTTP 200. Transport errors,
429, `RATELIMITED` и 5xx повторяются с bounded backoff; auth/permission и
malformed responses завершают текущую попытку.

## 7. Ошибки и безопасность

При недоступности Linear применяется fail-open:

- ни один GitLab MR не скрывается;
- используется стандартная GitLab-классификация;
- digest получает warning и status `partial`;
- неизвестный счётчик Linear не показывается как ложный ноль.

Если GitLab и Linear недоступны одновременно, run получает `failed`, сообщение
не создаётся. API key хранится как `config.Secret`, доступен через YAML/env/Vault
и регистрируется в redactor.

## 8. Хранение и идемпотентность

Linear issues не зеркалируются в PostgreSQL. В `digest_runs` хранится только
`linear_issue_count`; окончательные Block Kit payloads находятся в
`digest_messages`.

- повтор того же `(team, run_date, slot, attempt)` возвращает сохранённый run;
- `slack_send` retry не перечитывает GitLab или Linear;
- `digest --force` создаёт новый attempt и актуальный снимок обеих систем.

## 9. Doctor и метрики

`doctor` проверяет Linear authentication, каждый team UUID и наличие ровно
одного workflow state `In Review` без учёта регистра.

Метрики:

- `ai_reviewer_linear_requests_total{operation,result}`;
- `ai_reviewer_linear_request_duration_seconds{operation}`;
- `ai_reviewer_linear_issues_in_review{team}`;
- `ai_reviewer_digest_source_errors_total{team,source}`.

Identifiers и titles не используются как Prometheus labels.

## 10. Проверки реализации

- title имеет приоритет над branch только для реально найденной Linear issue;
- поиск работает при смешанном регистре и с несколькими tracker keys;
- zero approvals всегда сохраняет reviewer flow;
- один approval завершает review связанного MR;
- approval + `In Review` создаёт author-action перемещения карточки;
- любой `REQUESTED_CHANGES` отменяет completion независимо от approvals/status;
- отсутствие issue и отказ Linear работают fail-open;
- отдельные Linear rows не рендерятся, счётчик включает все `In Review` issues;
- batch lookup, pagination, team boundary, persistence и Slack limits покрыты
  race- и PostgreSQL integration tests.

Полная проверка: `make test-db`, `make lint`, затем dry-run с
`AI_REVIEWER_SERVICE_SLACK_SEND_ENABLED=false`.
