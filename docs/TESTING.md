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

001 проверяет совместимость River InsertTx/pgx/PostgreSQL 17, rollback общей транзакции, lockfile и локальные Compose/почту/S3. 002 проверяет sessions/CSRF, одноразовые токены и privacy job. 003/006/007 проверяют runtime sandbox, лицензионный набор и реальные файлы, статический профиль PDF, staging/TTL. 004/005 проверяют OpenAPI/DDL, cross-owner FK, дедупликацию, таймаут, отмену, fencing и восстановление. 008 проверяет редактор, шаблоны, snapshot и WCAG 2.2 AA. 009 закрывает возврат после вкладки, аналитические события и согласие. 010 проверяет сквозной экспорт/удаление, нагрузку, restore, почту, production-подобную топологию и независимое ревью. HEIC/HEIF проверяются в 007A, ZIP — в 015.

Для первой нагрузочной пробы использовать 20 активных клиентов, 5 JSON запросов/с, 2 параллельные загрузки, запуск каждые 10 секунд в течение 30 минут. Отдельно проверить максимальные входы и насыщение, зафиксировать характеристики узла, распределение сценариев, p95 по типам и ошибки. Цели REQUIREMENTS остаются стартовыми. LCP ≤2,5 с проверять на landing и точке входа в редактор на задокументированном мобильном профиле. При проверке доступности измерять keyboard, screen reader, reflow и контраст в обеих темах.

## TASK-002

Интеграционные тесты `backend/internal/accounts` требуют изолированный PostgreSQL 17 с применёнными миграциями и DSN ролей API, worker и migrate. Выполните `go test -race -mod=readonly -count=1 -v ./internal/accounts` из backend. Проверки охватывают регистрацию, outbox, CSRF, смену credentials, двух владельцев и privacy lifecycle.

На поднятом локальном Compose запустите из frontend `pnpm exec playwright install chromium` и `pnpm e2e`. Тест использует порт 8080 для UI/API и 8025 для Mailpit. Поддерживаются `AUTOLIFE_E2E_ORIGIN` и `AUTOLIFE_E2E_MAILPIT` для эквивалентного изолированного стенда. `AUTOLIFE_E2E_CHROMIUM` позволяет указать путь к уже установленному Chromium.

## TASK-003.01

Для интеграционных тестов используйте изолированный PostgreSQL 17 с миграциями и приватный bucket `autolife` на тестовом SeaweedFS. Задайте DSN ролей API и migrate, `AUTOLIFE_TEST_S3_ENDPOINT` и тестовые S3 credentials. Из backend выполните `go test -race -mod=readonly -count=1 -v ./internal/fileflow`. Тесты создают синтетические объекты и удаляют их по завершении.

В TASK-003 сначала проверяются изоляция launcher (.05) и инспекция (.06). Приёмка upload (.02) требует успешной проверки обеих подзадач и отдельного теста недоступности или ошибки инспекции до публикации.

## Матрица TASK-007A

- Проверять каждый вход и каждую заявку на выход реальными синтетическими fixtures на целевых Linux-сборках. Сравнивать текст, страницы, размеры, ориентацию, изображения, таблицы, алфавиты, фон, цвет и удаление метаданных; проверять порядок многостраничного вывода и округление DPI. Для CSV проверять кавычки, разделители, формулы, пустые поля и ведущие нули.
- Unit и integration: MIME mismatch, legacy OLE/ZIP, malformed, encrypted/password, макросы, активные HTML/SVG, отсутствие локальных/внешних запросов, распаковка сверх лимитов, symlink, ошибки процесса, профили и отмена/timeout. Проверить границу каждого лимита и превышение на единицу, очистку и изоляцию двух владельцев.
- E2E TASK-008: каталог и фильтры, одиночный/пакетный быстрый запуск, прогресс и возврат после закрытия, безопасный preview и download, многошаговая цепочка, ошибки RU/EN, мобильный UI, обе темы и клавиатура. TASK-009 сверяет статистику с первичными записями, включая retry и дубли.
- TASK-010 проверяет digest/SBOM/лицензии и целевой runtime, cold/warm benchmark, конкуренцию профилей и saturation, стабильность принятия задач, CPU/RAM/диск/S3/трафик, восстановление и откат capability. Для HEIC/HEIF публикуется матрица проверенных container/codec/архитектур и отдельное решение о выпуске.

## TASK-003 — локальная проверка и релизный допуск

- На мигрировавшем тестовом PostgreSQL 17 запустить `go test -race -mod=readonly -count=1 -v ./internal/fileflow ./internal/accounts` из backend с DSN API, worker и migrate. Тестовый инспектор компилируется только в тесты. Для отдельной проверки SeaweedFS задать тестовый endpoint и credentials, затем запустить `TestS3PrivateObject`.
- Собрать из корня `docker build -f backend/cmd/file-inspector/Dockerfile --target image -t autolife-task003-image backend` и соответствующую цель `pdf` с тегом `autolife-task003-pdf`. `python3 infrastructure/smoke/file-inspectors.py` (тестовая среда с Pillow) проверяет поведение парсеров в одноразовых контейнерах на синтетических файлах.
- Проверки парсеров на Docker Desktop и адаптеров в Go отделены от допуска launcher. В TASK-010.02 и .09 требуются реальный Linux с AppArmor/cgroup v2, подтверждённые digest профилей, реальный `TestLinuxRuntime`, проверка production-подобного маршрута загрузки через launcher, полный прогон confinement/cleanup. Пропуск любого теста из этого набора блокирует публичный релиз.

## TASK-004 — определения и реестр

- `go test -race -mod=readonly -count=1 ./internal/workflow/...` проверяет цепочку со всеми типами привязок, схему и версии, циклы/ссылки вперёд, конфигурацию и форматы, проверку output и недоступные обработчики.
- `TestWorkflowAPIAndPersistence` требует миграции 000009 и test DSN для API и migrate. Проверяются session/CSRF, матрица двух владельцев, черновики и копирование, атомарная конкурентная публикация, составные FK/RLS, неизменяемая версия/снимок, страницы, Outbox и архив. `TestPrivacyLifecycle` с DSN worker дополнительно проверяет экспорт и каскадное удаление определений.
- Полевые проверки изоляции FileFlow остаются в TASK-010. Реальное выполнение цепочки через очередь проверяется в TASK-005 и последующих этапах FileFlow. В TASK-004 цепочка использует handlers из тестов.

## TASK-005

С чистой схемой PostgreSQL 17 и DSN трёх тестовых ролей применить миграции приложения. `go test -race -mod=readonly -count=1 -v ./internal/execution` применяет официальные миграции River на тестовой БД и проверяет атомарный rollback, исполнение цепочки, checkpoint, idempotency, retry, cancel, квоты, staging, истечение ссылок и Outbox. Фактический launcher для изолированных действий и проверка Linux/AppArmor/cgroup v2 остаются обязательными в TASK-010.02/.09. Тестовые handlers и подтверждение остановки определены только в тестах.

## TASK-006

Собрать `docker build -t autolife-task006-image backend/cmd/image-transform`. `python3 infrastructure/smoke/image-transform.py` проверяет упаковку и качество на синтетических JPEG/PNG/WebP с Pillow. Для `TestImageWorkflows` нужны DSN ролей API/worker/migrate, отдельный SeaweedFS и `AUTOLIFE_IMAGE_TEST_TAG=autolife-task006-image` для реальной цепочки. Тестовый адаптер запускает тот же бинарник в ограниченных контейнерах, без использования ослабленного профиля в runtime приложения. Для общего прогона по одной тестовой БД используйте `go test -p 1 -race`: реестры fixture-пакетов различаются, каждому нужен свой прогон River. Проверка operator launcher с AppArmor/cgroup v2 остаётся в TASK-010.02/.09.

## TASK-007

Собрать `docker build -f backend/cmd/pdf-transform/Dockerfile -t autolife-task007-pdf backend`. `python3 infrastructure/smoke/pdf-transform.py` использует Pillow и pypdf для фикстур и проверки результата реального бинарника. `TestPDFWorkflowAPI` требует отдельные PostgreSQL и S3, а также `AUTOLIFE_PDF_TEST_TAG` и `AUTOLIFE_IMAGE_TEST_TAG`. Тест использует реальные маршруты, PostgreSQL/River, отдельное S3 и тестовый launcher для выполнения проверенных контейнеров. При общей тестовой очереди запускать интеграционные пакеты с `-p 1`. Операторский Unix launcher с AppArmor/cgroup v2 проверяется в TASK-010.
