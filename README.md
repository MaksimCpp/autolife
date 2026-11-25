# AutoLife

No-code платформа автоматизации повседневных задач. Первый публичный релиз — FileFlow. [TASK-001](docs/tasks/TASK-001.md) добавляет начальные Go-сервисы, frontend и локальную инфраструктуру.

## Локальный запуск

Для запуска требуются Docker с Compose v2 и OpenSSL. Docker и CI используют сохранённый `frontend/pnpm-lock.yaml`. Для проверок вне Docker используйте Go 1.27.4, Node.js 24 и Corepack с pnpm 10.30.3.

```sh
./infrastructure/compose/create-dev-env.sh
docker compose config --quiet
docker compose up --build --wait
```

Скрипт создаёт закрытый локальный `.env` со случайными credentials. Пример формата и параметры находятся в [.env.example](.env.example). На существующем окружении сохраните `.env`: пароли ролей PostgreSQL создаются при инициализации пустого тома, изменение файла само по себе их не ротирует. Перед первым запуском API и worker одноразовый контейнер `migrate` последовательно применяет встроенные миграции приложения, официальные миграции River и выдаёт роли API право `UPDATE (kind)` на `queue.river_job` для `InsertTx`. Повторный локальный запуск миграций выполняется командой `docker compose run --rm migrate`.

- UI и API через Caddy доступны на `http://127.0.0.1:8080`. Проверки API — `GET /health/live` и `GET /health/ready`. Readiness проверяет доступность базы и ожидаемой схемы. Контракт — [OpenAPI](docs/openapi.yaml).
- Mailpit UI — `http://127.0.0.1:8025`, тестовый SMTP — порт `1025` на loopback. SeaweedFS S3 — `http://127.0.0.1:8333` с локальными credentials. PostgreSQL доступен внутри сети Compose.
- `docker compose down` останавливает окружение, сохраняя локальные данные. `docker compose logs api worker migrate` показывает JSON-логи и статус миграций.

## Проверки

```sh
(cd backend && test -z "$(gofmt -l .)" && go mod verify && go vet -mod=readonly ./... && go test -mod=readonly -race ./... && go build -mod=readonly ./cmd/...)
(cd frontend && pnpm lint && pnpm typecheck && pnpm test && pnpm build && pnpm audit --audit-level high)
```

После запуска Compose установите AWS CLI v2 и Python 3 для smoke-теста, затем выполните:

```sh
set -a
. ./.env
set +a
./infrastructure/smoke/compose.sh
```

Smoke повторяет миграции, проверяет River InsertTx, отключает и восстанавливает **локальную** БД для проверки readiness, проверяет локальную доставку почты и приватные S3-операции на синтетических данных.

Go format, vet, unit/race, проверки хешей модулей, сборка и статическая проверка Compose выполнены. На локальном PostgreSQL 17.9 прошли первая и повторная миграции приложения и River CLI, проверка readiness при остановке и перезапуске БД и тест River InsertTx. Результаты проверок этапа, включая открытые замечания сканирования зависимостей, записаны в [TASK-001](docs/tasks/TASK-001.md).

## Документация

[Продукт](docs/PRODUCT.md), [требования](docs/REQUIREMENTS.md), [архитектура](docs/ARCHITECTURE.md), [стек](docs/TECH_STACK.md), [безопасность](docs/SECURITY.md), [ADR](docs/adr) и [ROADMAP](docs/ROADMAP.md). Правила работы находятся в [AGENTS.md](AGENTS.md). Production deploy, платные сервисы и внешние аккаунты требуют отдельного разрешения.
