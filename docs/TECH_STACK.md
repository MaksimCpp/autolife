# Технологический стек (согласованный выбор)

| Слой | Выбор | Обоснование |
|---|---|---|
| Backend | Go, net/http + chi | Компактный HTTP API, знакомый стек |
| DB | PostgreSQL 17, pgx v5, golang-migrate | Транзакции, JSONB, реляционные данные |
| Queue | River InsertTx + PostgreSQL | Durable jobs, enqueue в транзакции; меньше инфраструктуры |
| Frontend | Next.js, React, TypeScript, Tailwind CSS, shadcn/ui, pnpm | Адаптивный интерфейс и публичные страницы |
| Workflow editor | React Flow | Граф и связи; MVP линейный |
| Data fetching / forms | TanStack Query, React Hook Form, Zod | Кэширование и клиентская валидация, сервер остаётся источником истины |
| i18n/themes | next-intl, next-themes или эквивалент | RU/EN, dark/light/system |
| Storage | S3-compatible, локально SeaweedFS как основной кандидат после S3 contract tests | Приватные объекты, lifecycle, presigned links |
| Images/PDF | libvips, qpdf, img2pdf или проверенный эквивалент | Специализированная обработка в sandbox |
| Later files | Tesseract OCR, FFmpeg, Excelize | OCR, медиа, XLSX |
| Web | Go HTTP client, HTML parser, Playwright для JS при необходимости | Дешёвые проверки сначала, изолированный browser позже |
| AI | OpenAI API через ограниченный adapter | Structured output, лимиты и подтверждение |
| Auth | Argon2id, opaque sessions, secure cookies | Отзыв сессий и контроль сервера |
| Observability | slog, Prometheus, Grafana, OpenTelemetry при необходимости | Логи, метрики, tracing |
| Analytics | PostgreSQL, Recharts; PostHog/ClickHouse позднее | Не вводить второе хранилище без нагрузки |
| Test | Go testing, Testcontainers, Vitest, Playwright | Unit/integration/E2E |
| Infra | Docker Compose, Caddy, Linux VPS, GitHub Actions | Простота эксплуатации; изоляция задач отдельно |

## Правила выбора
До implementation зафиксировать актуальные стабильные версии и lockfiles, совместимость River/pgx/migrations, лицензии codec и images, статус поддержки и CVE. Не считать список проверенными актуальными версиями. Не добавлять Kafka, Redis, Kubernetes, gRPC без ADR с потребностью. Не устанавливать библиотеки будущих модулей в MVP. Не передавать Docker socket приложению/обработчику; отдельная безопасная граница запуска sandbox. Выбор email, object storage, VPS, OAuth и AI провайдеров после проверки доступности, стоимости и юрисдикции.

## Воспроизводимость

Точные поддерживаемые версии выбираются на этапах использования после проверки совместимости и лицензий. Фиксировать Go toolchain, go.mod/go.sum, pnpm и pnpm-lock.yaml, digest базовых образов, версии и хеши native/Python пакетов, SHA CI actions. CI использует замороженный lockfile. TASK-001 проверяет PostgreSQL 17, pgx, River InsertTx и миграции, frontend peer dependencies, Caddy/Compose и локальный S3. TASK-006/007 проверяют сборки libvips/qpdf/img2pdf и лицензии итоговых образов. HEIC включается в TASK-015 только после проверки HEVC-декодера, целевых платформ и лицензий. Dev использует тестовую почту. Production и внешние провайдеры проходят региональную, договорную и техническую проверку перед разрешённой интеграцией.
