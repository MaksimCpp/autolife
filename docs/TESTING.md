# Testing strategy

## Уровни
Go unit: validators, registry, typed bindings, state transitions, retries, quotas, permissions, deadlines. Integration Testcontainers: PostgreSQL migrations/FKs, River enqueue transaction, concurrent workers, idempotency, crash recovery, S3 storage/TTL. File fixtures: JPEG/PNG/WebP/PDF, invalid bytes, orientation/alpha, ranges, large dimensions, parser failure; compare properties rather than unstable binary exactness. Frontend Vitest: form validation, status mapping, locale/theme, API errors. Playwright E2E: register/verify/login with test mail, upload→template→run→close/reopen→download, custom chain, invalid config, cancellation, expired file, both themes and mobile.

## Failure and security tests
Two owners cannot access each other's workflows/files/executions; race quota/duplicate idempotency keys; worker crash before/after commit; storage failure and orphan cleanup; cancellation and timeout; malformed files, path traversal, decompression bomb, sandbox network/secret isolation. Future: fake clock DST/schedule, local test HTTP SSRF/redirect, mock OAuth/cursor/webhook, prompt injection and malformed AI outputs. No external prod requests/credentials; fixtures synthetic.

## Load/quality
Record hardware, dataset, concurrency, p95 API/queue, resource usage and errors. Test normal and saturation; backpressure/fairness. Preliminary goals REQUIREMENTS.md; do not claim validated until measured. CI: gofmt, go vet, go test ./..., race tests for critical concurrency, lint, frontend typecheck/lint/test/build, integration and E2E in suitable environment, migration smoke, dependency/secret scan. Pin versions. Report command, exit code, limitations; cannot mark passing if not run. Regression tests for bug fixes. No disabling tests to pass CI.

## Release gate
All P0 acceptance scenarios verified, backup restore tested, HTTPS, monitoring, sandbox controls, privacy/terms, zero known critical/security and blocking defects. Human review of auth, file isolation and deployment. Staging smoke after migrations; rollback procedure tested.

## Обязательные ворота по этапам

001 проверяет совместимость River InsertTx/pgx/PostgreSQL 17, rollback общей транзакции, lockfile и локальные Compose/почту/S3. 002 проверяет sessions/CSRF, одноразовые токены и privacy job. 003/006/007 проверяют runtime sandbox, лицензионный набор и реальные файлы, статический профиль PDF, staging/TTL. 004/005 проверяют OpenAPI/DDL, cross-owner FK, дедупликацию, таймаут, отмену, fencing и восстановление. 008 проверяет редактор, шаблоны, snapshot и WCAG 2.2 AA. 009 закрывает возврат после вкладки, аналитические события и согласие. 010 проверяет сквозной экспорт/удаление, нагрузку, restore, почту, production-подобную топологию и независимое ревью. HEIC и ZIP проверяются отдельно в 015.

Для первой нагрузочной пробы использовать 20 активных клиентов, 5 JSON запросов/с, 2 параллельные загрузки, запуск каждые 10 секунд в течение 30 минут. Отдельно проверить максимальные входы и насыщение, зафиксировать характеристики узла, распределение сценариев, p95 по типам и ошибки. Цели REQUIREMENTS остаются стартовыми. LCP ≤2,5 с проверять на landing и точке входа в редактор на задокументированном мобильном профиле. При проверке доступности измерять keyboard, screen reader, reflow и контраст в обеих темах.

## TASK-002

Интеграционные тесты `backend/internal/accounts` требуют изолированный PostgreSQL 17 с применёнными миграциями и DSN ролей API, worker и migrate. Выполните `go test -race -mod=readonly -count=1 -v ./internal/accounts` из backend. Проверки охватывают регистрацию, outbox, CSRF, смену credentials, двух владельцев и privacy lifecycle.

На поднятом локальном Compose запустите из frontend `pnpm exec playwright install chromium` и `pnpm e2e`. Тест использует порт 8080 для UI/API и 8025 для Mailpit. Поддерживаются `AUTOLIFE_E2E_ORIGIN` и `AUTOLIFE_E2E_MAILPIT` для эквивалентного изолированного стенда. `AUTOLIFE_E2E_CHROMIUM` позволяет указать путь к уже установленному Chromium.
