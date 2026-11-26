# Database — концептуальная схема PostgreSQL

UUID PK для публичных сущностей; timestamptz UTC; даты без времени отдельно; owner_id индексируется. FK и CHECK для допустимых статусов, размеров, валют и счётчиков. JSONB только для версионированных схем/метаданных, не для скрытия всех отношений. Миграции up/down где безопасно; destructive rollback через backup/forward fix. Точные DDL и индексы утверждаются перед задачей.

## MVP таблицы
| Таблица | Основные поля/ограничения |
|---|---|
| users | id, normalized_email UNIQUE, password_hash, email_verified_at, status, deleting_at, created_at |
| user_settings | user_id PK/FK, locale, timezone, theme, notifications, analytics_consent, analytics_consent_updated_at |
| sessions | id, user_id, token_hash UNIQUE, expires_at, revoked_at, last_authenticated_at |
| auth_tokens | user_id, token_hash UNIQUE, purpose, expires_at, used_at |
| workflows | id, owner_id, name, description, status, active_version_id, timestamps |
| workflow_drafts | workflow_id PK, revision, definition JSONB, updated_at |
| workflow_versions | id, workflow_id, version, schema_version, definition JSONB, UNIQUE(workflow_id,version), immutable |
| executions | id, owner_id, workflow_id, version_id или snapshot_id, status, trigger_type, queued_at, start_deadline, started_at, run_deadline, terminal_at, error_code, lease_until, fence |
| execution_steps | id, execution_id, step_key, status, action_type/version, typed binding metadata, timestamps, UNIQUE(execution_id,step_key) |
| execution_attempts | step_id, number CHECK 1–3, outcome, fence, retry_at, timestamps, UNIQUE(step_id,number) |
| files | id, owner_id, object_key UNIQUE, display_name, mime, bytes, sha256, state, ready_at, expires_at |
| execution_file_refs | execution_id, file_id, role, slot, step_id optional, pin state/expiry; unique logical reference по role/slot, owner_id для составных FK |
| draft_execution_snapshots | id, workflow_id, owner_id, revision, schema_version, immutable definition |
| privacy_jobs | id, owner_id, type, state, result_file_id, requested_at, completed_at, expires_at, safe_error |
| templates | id, slug UNIQUE, version, locale metadata, definition, state |
| quota_limits | plan or owner scope, resource_type, value |
| quota_reservations | id, owner_id, scope/type/reference, execution_id nullable, resource, reserved, state, expiry, unique logical reservation |
| idempotency_keys | owner_id, operation, key, request_hash, result_ref, expires_at, UNIQUE(owner_id,operation,key) |
| notifications | id, owner_id, type, status, safe_payload, created_at/read_at |
| outbox_events | event_id PK, type, schema_version, logical_key, occurred_at, allowlisted payload, UNIQUE(type,logical_key) |
| outbox_consumptions | consumer, event_id, processed_at, PRIMARY KEY (consumer,event_id) |
| analytics_events | event_id PK, type, timestamp, actor_ref, allowed properties |
| analytics_daily | day, scope, metric, dimensions, value; deterministic unique key |
| audit_events | id, actor, action, resource_ref, outcome, occurred_at; request_id, no secrets |
| River tables | managed through official migration procedure |

## Future tables
monitors/checks/baselines; integration_connections/credential_refs/sync_cursors; inbox_message_refs/processing_records; obligations/occurrences/reminders/extraction_candidates; approval_requests; subscription_plans/subscriptions/billing_events; workspaces/memberships/marketplace templates. Add only when stage requires them. Composite uniqueness for trigger event dedup and provider webhook ID. No plaintext OAuth tokens. Owner checks must include nested relations; consider composite FKs or RLS as defense in depth with tests, not substitute for auth.

## Queries and lifecycle
Indexes: owner+created_at/id cursor, execution status+queued_at, expiry, event time, monitor next_check; evaluate EXPLAIN. Quota transactions lock relevant owner counters/reservations; concurrent tests. Historical version references preserved while executions exist. Delete account revokes sessions, stops new jobs, cleans files and future connections; explicit deletion job/reconciliation, minimal legally required records only. Retention per REQUIREMENTS/ANALYTICS. Backup encrypted, restore drill, lifecycle orphan sweep. Never delete active input refs. Money minor units/decimal and currency.

## Инварианты и жизненный цикл

Составные UNIQUE/FK связывают owner и ресурс. executions(owner_id,workflow_id) → workflows, (workflow_id,version_id) → workflow_versions либо (owner_id,workflow_id,snapshot_id) → draft_execution_snapshots. CHECK требует ровно один источник определения. active_version связан с версией того же workflow. execution_file_refs связывает (owner_id,execution_id), (owner_id,file_id) и при наличии (execution_id,step_id). Приватные результаты экспорта также связаны с owner. Приложение валидирует права до каждого обращения.

Реляционные file refs — источник истины для ACL, pin и порядка. JSONB хранит версии и метаданные bindings, но не заменяет ограничения доступа. Удаление и выдача pin сериализуются по строке файла. Физическая очистка проходит через durable deleting и повторяемое удаление S3. Пока существуют ссылки истории, остаётся минимальная запись expired без имени и ключа объекта. Новые pin и URL недоступны. После истечения истории запись удаляется. При удалении аккаунта ссылки и tombstone того же владельца удаляются в согласованном порядке. Самостоятельный lifecycle S3 применяется только к staging/orphan по политике, совместимой с живыми ссылками. Quota counters блокируются в фиксированном порядке, резервы и фактические значения сверяются после crash. Историческая версия или снимок сохраняются вместе с execution. Ротация токенов и использование reset/verify атомарны.

Privacy job имеет requested/running/ready/failed/expired для экспорта и requested/running/succeeded/failed для удаления, с повторяемой обработкой и безопасной ошибкой. Сведения удаления для restore хранятся отдельно минимальным набором согласно ADR-002. Точные DDL, миграции и тесты отрицательных случаев утверждаются перед реализацией таблиц.

## Реализация TASK-002

Миграции 000002–000006 создают аккаунты, настройки, сессии, токены, outbox, аудит, privacy jobs и счётчики авторизации. Пользовательские строки и экспорт удаляются каскадно. Запись запроса удаления сохраняется для идемпотентного подтверждения, хеш квитанции очищается через 24 часа. Результат экспорта хранится в поле `privacy_jobs.result` как JSON с TTL 24 часа. Worker удаляет истёкшие данные. Приложение дополняет audit только вставками. Очистка старше 90 дней выполняется функцией с фиксированным порогом, доступной worker.

## Файловая модель TASK-003.01

Миграция 000007 добавляет `platform.files`, `file_pins`, `quota_limits` и `quota_reservations`. PostgreSQL формирует ключ объекта из UUID файла. Файл в состоянии `ready` имеет MIME из разрешённого списка, размер от 1 до 100 000 000 байт, SHA-256, время готовности и срок хранения. Роль API ограничена политикой RLS с контекстом владельца в пределах транзакции. Pin и резервирование используют составной FK `(owner_id, file_id)`. Публичные файловые маршруты и учёт квот подключаются следующими подзадачами этапа.

## Расширение форматов TASK-007A

Отдельная аддитивная миграция расширяет серверный allowlist MIME в `platform.files`. Старые строки и ссылки не переписываются; исходники, результаты и растровые производные используют существующие UUID, owner, RLS, pins, staging, quota и TTL. MIME определяется после инспекции, при необходимости хранится нормализованный идентификатор формата для различения контейнера и кодека. Миграция проверяется на пустой и существующей БД. При откате приложения сохранить читаемость новых записей, выключить новые операции; обратное сужение допускается только после проверки отсутствия несовместимых строк.

Порядок описывается выходными refs шага и безопасными индексами источника/страницы, а не порядком строк в SQL. Измерения попыток привязываются к execution/step/attempt, логический исход — к step; применять согласованные ACL, retention и дедупликацию по ANALYTICS.

Для быстрого запуска TASK-007A.07 добавляет immutable `quick_execution_snapshots` с составным ключом (owner_id, id), схемой и одним действием. В executions добавляется owner-scoped ссылка quick_snapshot_id. CHECK допускает ровно один источник определения: опубликованную версию с workflow_id, снимок черновика с workflow_id либо снимок быстрого запуска с NULL workflow_id. Существующие составные FK сохраняются; для двух веток с workflow_id требуется его наличие. История, retry, удаление владельца и восстановление охватывают все три источника. Проверить миграцию на старых данных и запрет чужих/смешанных ссылок.

## Жизненный цикл файлов TASK-003

Миграция 000008 добавляет scoped grants/RLS для резерва и файлов, рабочую роль очистки и проверку новых pin через блокировку строки файла. Параллельный резерв сериализован по владельцу, резерв 100 000 000 байт заменяется фактическим размером после записи объекта. Политика использует значения quota_limits либо настройки оператора. Outbox и переход в ready коммитятся вместе. Worker очищает истёкшие и незавершённые записи, повторяет удаление после сбоев и постранично сверяет старые S3-объекты из собственного префикса. Удаление аккаунта ждёт файловую очистку. Досрочное удаление файлов аккаунта начинается из durable running delete job после подтверждения журнала восстановления.

## Жизненный цикл автоматизаций TASK-004

Миграция 000009 добавляет workflows, drafts, versions и draft execution snapshots. Транзакции API устанавливают owner-контекст под RLS. Составные FK связывают строки с владельцем и workflow, включая откладываемый FK активной версии. Редактирование сериализуется по владельцу и workflow. Одна ревизия черновика соответствует максимум одной опубликованной версии; номер версии возрастает внутри workflow. Триггеры запрещают UPDATE сохранённых версий и снимков. Снимок черновика не меняет active_version_id. Архив сохраняет историю. Удаление аккаунта удаляет все связанные определения каскадно; приватный экспорт включает черновики, опубликованные версии и внутренние снимки владельца. Down разрешён только при отсутствии workflow.
