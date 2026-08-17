# План интеграции Linear в digest

## 1. Цель

Добавить Linear как read-only источник данных для командного Slack-digest. В
digest должны попадать только неархивные Linear issues, которые в момент сборки
находятся в workflow status `In Review`.

API key задаётся через существующую систему конфигурации и секретов. Сервис не
создаёт и не изменяет issues в Linear и не зеркалирует их в PostgreSQL.

## 2. Рекомендуемое продуктовое поведение

Рабочая гипотеза до подтверждения владельцем продукта:

- существующие GitLab-блоки digest сохраняются;
- после них выводится отдельный блок `Linear · In Review`;
- каждая команда приложения явно связывается с одной или несколькими Linear
  teams по UUID;
- Linear issue выводится одной строкой: identifier, title, assignee (если есть)
  и ссылка;
- issues без assignee тоже выводятся, отдельной группой `Unassigned`;
- если подходящих issues нет, пустой Linear-блок не показывается;
- если Linear временно недоступен, GitLab-часть всё равно отправляется с
  предупреждением о неполных данных, а `digest_runs.status` становится
  `partial`.

Linear issues не следует смешивать с существующими `ToReview`/`Own` buckets:
эти buckets описывают действия над GitLab MR, а assignee Linear issue не
обязательно является человеком, который должен выполнить code review.

## 3. Конфигурация

Предлагаемая схема:

```yaml
linear:
  api_key: "" # AI_REVIEWER_LINEAR_API_KEY; в production — env или Vault
  endpoint: https://api.linear.app/graphql
  timeout: 15s
  max_attempts: 4
  max_retry_after: 60s

teams:
  - name: payments
    slack_channel: C012345678
    linear_team_ids:
      - 9cfb482a-81e3-4154-b5b9-2c805e70a02d
    repositories: [backend/payments, backend/billing]
```

UUID предпочтительнее имени или team key: отображаемое имя и key могут быть
переименованы, UUID остаётся стабильной привязкой. UUID можно скопировать в
Linear через command menu → `Copy model UUID`.

Правила конфигурации:

- Linear включён для конкретной команды, если у неё непустой
  `linear_team_ids`;
- `linear.api_key` обязателен, только если хотя бы одна команда использует
  Linear;
- один Linear team UUID не может принадлежать двум командам приложения, иначе
  одна issue попадёт в два Slack-канала;
- endpoint должен быть абсолютным HTTPS URL; HTTP допустим только в тестах;
- API key хранится как `config.Secret`, регистрируется в redactor и никогда не
  логируется;
- имя статуса не делается пользовательской настройкой: продуктовый инвариант —
  именно `In Review`. Сравнение выполняется без учёта регистра и с trim;
- отсутствие `linear_team_ids` сохраняет текущее поведение без Linear и не
  требует ключа.

Изменения затронут `internal/config`, `config.example.yaml`,
`docs/configuration.md`, `docs/installation.md` и doctor diagnostics.

## 4. Linear API

Использовать официальный GraphQL endpoint `https://api.linear.app/graphql` и
personal API key в заголовке `Authorization: <API_KEY>` — без префикса
`Bearer`. Для текущего self-hosted сервиса OAuth не нужен: интеграция работает
от одного явно настроенного технического аккаунта и выполняет только чтение.

Для каждой команды приложения выполняется один пагинируемый запрос по всем её
Linear team UUID. Фильтрация должна происходить на стороне Linear, а не после
загрузки всех issues. Концептуально запрос выглядит так (точные input-типы
нужно зафиксировать по introspection текущей схемы при реализации):

```graphql
query IssuesInReview($teamIds: [ID!]!, $after: String) {
  issues(
    first: 50
    after: $after
    filter: {
      team: { id: { in: $teamIds } }
      state: { name: { eqIgnoreCase: "In Review" } }
    }
    orderBy: updatedAt
  ) {
    nodes {
      id
      identifier
      title
      url
      updatedAt
      state { id name }
      team { id key name }
      assignee { id name email }
    }
    pageInfo { hasNextPage endCursor }
  }
}
```

После ответа клиент повторно проверяет `state.name == In Review` без учёта
регистра. Это защитный барьер: даже при ошибке фильтра или изменении схемы issue
с другим статусом не должна попасть в digest. Archived resources Linear по
умолчанию не возвращает; `includeArchived` не включаем.

Пагинация идёт по `pageInfo.endCursor` до `hasNextPage=false`. Результат
дедуплицируется по issue UUID и локально сортируется стабильным образом:
Linear team key, затем identifier. На порядок, случайно возвращённый API,
рендер полагаться не должен.

Официальная документация:

- [GraphQL API и аутентификация](https://linear.app/developers/graphql)
- [Filtering](https://linear.app/developers/filtering)
- [Pagination](https://linear.app/developers/pagination)
- [Rate limiting](https://linear.app/developers/rate-limiting)

## 5. Клиент и границы пакетов

Добавить пакет `internal/linear` с небольшим собственным HTTP-клиентом. У
Linear нет официального Go SDK, а для одной read-only операции подключать
универсальный GraphQL runtime не требуется.

Пакет отвечает только за wire-протокол:

- формирование GraphQL request и decoding response;
- cursor pagination;
- заголовки аутентификации;
- timeout/cancellation;
- классификацию transport, HTTP, GraphQL и rate-limit ошибок;
- bounded retry для transport errors, HTTP 429 и 5xx;
- соблюдение `Retry-After`, ограниченного `max_retry_after`;
- проверку GraphQL `errors`, даже если HTTP status равен 200.

Интерфейс, используемый service layer, должен быть узким, например:

```go
type API interface {
    ListIssuesInReview(ctx context.Context, teamIDs []string) ([]Issue, error)
}
```

`internal/service` преобразует wire-модель Linear в данные digest. Пакет
`internal/slack` не должен импортировать `internal/linear`, а `internal/linear`
не должен знать о Slack, Postgres или River.

## 6. Встраивание в текущий BuildDigest

Текущая цепочка остаётся `digest → slack_send`; отдельная River job на запрос к
Linear не нужна. Снимок Linear должен соответствовать тому же моменту сборки,
что и снимок GitLab, а повторная доставка уже сохранённого сообщения не должна
повторно обращаться к внешним API.

Изменения по слоям:

1. `internal/domain.Team` получает `LinearTeamIDs []string`.
2. Composition root создаёт один thread-safe Linear client на процесс и
   передаёт его в `service.Deps`.
3. `Service.BuildDigest` независимо собирает GitLab и Linear sources.
4. Успешные Linear issues преобразуются в новый neutral render DTO, например
   `slack.LinearItem`, и добавляются в `slack.DigestData`.
5. Block Kit builder рендерит отдельную секцию и учитывает её при существующем
   разбиении по лимитам Slack.
6. Готовые Slack payloads сохраняются в `digest_messages`, как сейчас;
   `slack_send` только доставляет сохранённый payload.

Нужна небольшая переработка модели partial failure. Сейчас она знает только
`FailedRepos`; после интеграции лучше передавать в renderer структурированные
warnings (`GitLab: N repositories unavailable`, `Linear unavailable`), чтобы
не выдавать неполный digest за полный.

Правило результата сборки:

| GitLab source | Linear source | Результат |
|---|---|---|
| success | success или не настроен | `built` |
| success/partial | failed | `partial`, отправить доступные данные с warning |
| failed | success | `partial`, отправить Linear-данные с warning |
| failed | failed | `failed`, ничего не отправлять, River применяет retry |

Строка Linear должна проходить через те же escaping и size limits, что и MR:
title и имена приходят из внешней системы и не должны ломать Slack mrkdwn или
превышать section limit.

## 7. Хранение и идемпотентность

Linear issues не нужно хранить отдельными строками в Postgres. Digest является
снимком на момент запуска, а его окончательный Slack payload уже хранится в
`digest_messages`. Это сохраняет действующий протокол idempotency:

- повтор `BuildDigest` того же `(team, run_date, slot, attempt)` возвращает уже
  построенный run и не перечитывает Linear;
- retry `slack_send` отправляет тот же payload;
- ручной `digest --force` создаёт новый attempt и новый актуальный снимок.

Для наблюдаемости стоит добавить `linear_issue_count` в `digest_runs` отдельной
миграцией `0002_*`, не переименовывая существующий `mr_count`: старое поле имеет
понятную историческую семантику. После миграции нужно перегенерировать pgxgen
models/repos и добавить count в `DigestOutcome` и structured logs.

## 8. Ошибки, retry и rate limits

Linear API key сейчас ограничен 5 000 requests/hour; два digest-слота и один
пагинируемый запрос на настроенную команду дают большой запас. Тем не менее
клиент должен быть корректным при деградации:

- HTTP 401/403 и GraphQL auth/permission errors — fatal для текущей job attempt;
- HTTP 429 / GraphQL `RATELIMITED`, 5xx и transport errors — retry с bounded
  exponential backoff и jitter;
- malformed response или GraphQL response с `errors` — source failure;
- частичный GraphQL `data` вместе с `errors` не использовать: неполный список
  без явной границы опаснее, чем warning о недоступном Linear;
- context cancellation и deadline немедленно останавливают pagination/retry;
- в логах допустимы operation, team, page, status/error class и counts, но не
  request headers, raw request dump или API key.

Retry внешнего запроса выполняется внутри одной digest job, как уже сделано для
GitLab. River затем повторяет всю сборку только если не удалось получить ни
одного настроенного source.

## 9. Doctor и эксплуатация

Расширить `ai-reviewer doctor`:

1. Если Linear нигде не настроен — `SKIP linear (not configured)`.
2. Проверить наличие API key без его вывода.
3. Выполнить дешёвый `viewer` query, подтвердить authentication.
4. Для каждого `linear_team_ids` проверить существование и доступность team.
5. Убедиться, что у каждой team существует ровно один workflow state с именем
   `In Review` без учёта регистра. Ноль совпадений — ошибка конфигурации;
   неоднозначность — тоже ошибка, а не случайный выбор.
6. Показать безопасное резюме: app team → Linear team name/key → state name.

Добавить метрики с bounded labels:

- `ai_reviewer_linear_requests_total{operation,result}`;
- `ai_reviewer_linear_request_duration_seconds{operation}`;
- `ai_reviewer_linear_issues_in_review{team}`;
- `ai_reviewer_digest_source_errors_total{team,source}`.

Не использовать issue identifier, title, user id или GraphQL message как label.

## 10. Тесты

### `internal/linear`

- правильный endpoint, method, content type и API-key header;
- API key не появляется в ошибках и логах;
- server-side filter содержит только `In Review` и заданные team IDs;
- несколько cursor pages собираются полностью;
- issue с другим state отбрасывается защитной проверкой клиента;
- duplicate issue UUID возвращается один раз;
- HTTP 200 + GraphQL `errors` считается ошибкой;
- 401/403 не retry; 429, 5xx и transport error retry;
- `Retry-After` ограничивается конфигом;
- context cancellation останавливает retry и pagination.

### config / app / doctor

- пример конфигурации парсится;
- API key требуется только при наличии `linear_team_ids`;
- невалидные/повторно назначенные UUID отклоняются;
- secret регистрируется в redactor;
- client создаётся ровно один раз и корректно отсутствует без настройки;
- doctor различает skip, auth failure, missing team и missing `In Review` state.

### service / slack / jobs

- в digest попадают только Linear issues `In Review`;
- Linear section не меняет существующую GitLab-классификацию;
- пустой Linear result не создаёт пустую секцию;
- unassigned issue не теряется;
- порядок детерминирован;
- special characters и длинные titles безопасно рендерятся;
- pagination Slack messages учитывает одновременно GitLab и Linear blocks;
- Linear failure + GitLab success даёт persisted `partial` payload с warning;
- обе системы недоступны — run `failed`, messages отсутствуют;
- retry существующего run не делает второй Linear request;
- dry-run сохраняет Linear payload, но не создаёт `slack_send` job.

Полная проверка: `make test-db` и `make lint`.

## 11. Этапы реализации

1. Добавить config schema, validation, secret registration, example и docs.
2. Реализовать и протестировать `internal/linear` на `httptest.Server`.
3. Подключить client в composition root и doctor.
4. Расширить domain/service DTO и source failure model.
5. Добавить Linear section в Block Kit builder и тесты лимитов.
6. Добавить миграцию `linear_issue_count`, обновить pgxgen queries/models.
7. Добавить metrics, structured logs и operational documentation.
8. Прогнать `make test-db`, race suite и lint; затем включить сначала при
   `service.slack_send_enabled: false` и проверить сохранённый preview.

## 12. Критерии готовности

- Без Linear-конфигурации поведение и payload digest не меняются.
- При корректной конфигурации ни одна issue не попадает в digest, если её
  текущий status не равен `In Review` без учёта регистра.
- Все pages Linear API учитываются, дубликатов нет, порядок стабилен.
- Недоступность Linear видна как `partial`, но не скрывает доступную GitLab
  часть; недоступность всех configured sources не создаёт ложный digest.
- API key доступен через YAML/env/Vault, маскируется в логах и не попадает в
  persisted payload.
- Dry-run, idempotency BuildDigest и retry доставки остаются неизменными.
- Doctor заранее обнаруживает неверный key, team UUID или отсутствие нужного
  workflow state.

## 13. Вопросы перед реализацией

1. Linear-блок должен дополнять текущий GitLab digest (рекомендуемый вариант)
   или полностью заменить его?
2. Привязка к Slack-командам действительно проходит через Linear team UUID,
   или в вашем workspace граница задаётся Linear project/label?
3. Достаточен отдельный плоский блок с assignee, или Linear issues нужно
   группировать по людям и пытаться делать Slack mentions по email?
