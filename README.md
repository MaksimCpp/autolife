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

## Аккаунты

После запуска откройте `http://127.0.0.1:8080/ru/account/register`. Письма подтверждения и восстановления доступны в Mailpit. Для существующего локального `.env` добавьте `AUTOLIFE_AUTH_SECRET` со значением из 32 случайных байтов в hex и сохраните прежние настройки PostgreSQL. Новый `.env` создаёт этот ключ автоматически. При смене локального адреса установите точный `AUTOLIFE_PUBLIC_ORIGIN`.

Удаление хранит минимальный журнал в отдельном томе `restore-journal`. Сохраняйте этот том при операциях со снимками PostgreSQL. Браузерные тесты описаны в [TESTING](docs/TESTING.md).

## Файловое API

Метаданные файлов доступны авторизованному владельцу через `/api/v1/files`. Для загрузки оператор Linux отдельно запускает `sandbox-launcher` с двумя проверенными digest профилей и передаёт API путь к его сокету через `AUTOLIFE_FILE_INSPECTOR_SOCKET`. Сокет должен быть доступен API на чтение/запись по правам Unix; Docker socket остаётся вне API и worker. Локальный Compose по умолчанию не подключает launcher, поэтому загрузка отвечает 503 до настройки совместимого runtime. Тесты и обязательный релизный допуск описаны в [TESTING](docs/TESTING.md).

Для действий изображений соберите `docker build -t autolife-task006-image backend/cmd/image-transform`. На совместимом Linux оператор задаёт launcher опцию `-image-transform` с проверенным digest и монтирует только сокет launcher в API и worker, передавая его путь через `AUTOLIFE_IMAGE_LAUNCHER_SOCKET`. Каталог скрывает действия, пока профиль или приватное хранилище недоступны. Тесты форматов описаны в [TESTING](docs/TESTING.md).
