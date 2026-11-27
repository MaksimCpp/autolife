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
| Later files | Tesseract OCR, FFmpeg | OCR, медиа, XLSX |
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

Точные поддерживаемые версии выбираются на этапах использования после проверки совместимости и лицензий. Фиксировать Go toolchain, go.mod/go.sum, pnpm и pnpm-lock.yaml, digest базовых образов, версии и хеши native/Python пакетов, SHA CI actions. CI использует замороженный lockfile. TASK-001 проверяет PostgreSQL 17, pgx, River InsertTx и миграции, frontend peer dependencies, Caddy/Compose и локальный S3. TASK-006/007 проверяют сборки libvips/qpdf/img2pdf и лицензии итоговых образов. HEIC/HEIF проходит проверку декодеров, целевых платформ и лицензий в TASK-007A.06. Dev использует тестовую почту. Production и внешние провайдеры проходят региональную, договорную и техническую проверку перед разрешённой интеграцией.

## FileFlow — выбор инструментов

| Инструмент | Роль, совместимость и цена обработки | Лицензия и ограничения |
|---|---|---|
| LibreOffice Headless | Один движок Writer/Calc/Impress для MS Office, ODF и UTF-8 TXT. Холодный запуск и шрифты требуют замеров RAM/CPU, временный профиль отдельный на задание. | MPL-2.0 и компоненты под другими лицензиями, включая условия LGPL для соответствующего кода. Проверить полный состав образа. [Лицензии](https://www.libreoffice.org/licenses/) |
| libvips | Базовые изображения и кодирование PNG/JPEG; SVG через librsvg, HEIF через libheif и минимальную сборку decode-only с кандидатом libde265. Малый рабочий набор для ряда операций, сложные SVG и HEIF измеряются отдельно. | LGPL-2.1-or-later у libvips/libheif; проверить лицензии librsvg и декодера по версии, условия распространения бинарников и отдельно права на HEVC по территории и способу использования. [libvips](https://github.com/libvips/libvips), [libheif](https://github.com/strukturag/libheif), [libde265](https://github.com/strukturag/libde265), [librsvg](https://gitlab.gnome.org/GNOME/librsvg) |
| qpdf + img2pdf | qpdf инспектирует и преобразует структуру, img2pdf создаёт страницы из изображений без лишнего перекодирования там, где возможно. WebP нормализуется libvips. Проверить стоимость распаковки потоков, Pillow и пиковую память. | qpdf Apache-2.0 для выбранной современной ветки, img2pdf LGPL-3.0-or-later. [qpdf](https://github.com/qpdf/qpdf), [img2pdf](https://gitlab.mister-muffin.de/josch/img2pdf) |
| PDFium | Единственный растеризатор PDF, нативный закреплённый helper; отключить V8/XFA и проверить архитектуры, сборку и обновления. Рендер по одной странице, бюджет пикселей проверяется до аллокации. Poppler технически подходит, но добавляет GPL-обязательства при распространении. PDFBox под Apache-2.0 подходит, но для MVP добавляет JVM и её startup/RAM. | PDFium BSD-3-Clause и отдельные условия third-party. [PDFium](https://pdfium.googlesource.com/pdfium/+/refs/heads/main/LICENSE), [Poppler](https://poppler.freedesktop.org/), [PDFBox](https://pdfbox.apache.org/) |
| Chromium Headless | Только статическая HTML-печать для поддержки print CSS. Изолированный отдельный профиль, без JS/фоновых запросов и с рабочей встроенной песочницей. Измерять cold start, дочерние процессы, пик RAM и обновлять независимо от других конвертеров. | Chromium BSD-style плюс third-party notices; использовать сборку Chromium. [Лицензия](https://chromium.googlesource.com/chromium/src/+/refs/heads/main/LICENSE), [Headless](https://developer.chrome.com/docs/chromium/headless) |
| Excelize + Go encoding/csv | Excelize переносится в MVP только для XLSX ↔ CSV. Потоковый обход и фиксированная семантика значений, без пересчёта формул и без холодного старта офисного процесса. Проверить совместимость с Go проекта, лимиты shared strings, XML/ZIP, точность значений и память на больших листах. | BSD-3-Clause у Excelize, BSD-style у Go. [Excelize](https://github.com/qax-os/excelize), [Go](https://go.dev/LICENSE) |
| Tesseract | P1 OCR, модели rus/eng, PDFium для рендера PDF. Стоимость зависит от DPI, числа страниц и моделей. OCR запускается в отдельном профиле. | Apache-2.0, сверить модели и Leptonica отдельно. [Tesseract](https://github.com/tesseract-ocr/tesseract) |
| FFmpeg | P2, один профильный инструмент для медиа, allowlist кодеков/контейнеров, только выделенные файлы, замеры декодирования/кодирования и CPU по профилям. | LGPL-2.1-or-later по умолчанию, GPL при включении соответствующих компонентов; сборка без GPL/nonfree по умолчанию. Проверка патентов отдельно для фактических кодеков и территорий. [FFmpeg](https://ffmpeg.org/legal.html) |

Инструменты для PDF → Office и электронных книг выбираются в TASK-015 по сравнительным fixtures. Сначала проверить повторное использование имеющихся парсеров/писателей для извлечения и генерации. Фиксировать поддерживаемые версии, digest, SBOM, notices, обязанности предоставления исходников или перелинковки при распространении и лицензии шрифтов. Коммерческая эксплуатация предполагает выполнение применимых условий, включая отдельную проверку кодеков. Все конвертации выполняются на инфраструктуре AutoLife. Платные API и внешние конвертеры требуют отдельного согласования.

Сравнение выше качественное. На целевом Linux измерить cold/warm p50/p95, CPU-секунды, пик RAM, временный диск, S3-операции, байт-дни хранения, входящий/исходящий трафик и конкуренцию. Бюджет оценивать по наблюдаемой сумме вычислений, ёмкости/операций хранения и тарифицируемого трафика. При фиксированном VPS проверить требуемое число узлов и запас ёмкости; при недостаточной изоляции отдельно оценить выделенные узлы/VM. Численные тарифы определяются после выбора инфраструктуры.

Для сценарной оценки при 10 000 входных файлов в день, среднем размере входа 5 МБ и результата 8 МБ, сроке 7 дней и одном полном скачивании каждого результата понадобится около 910 ГБ полезного хранения и 80 ГБ исходящего трафика в день, без учёта резервных копий и промежуточных данных. При 10 CPU-секундах на файл это около 28 vCPU·ч в день до повторов. Это допущения для модели, значения заменяются результатами TASK-010.04.

### Проверенная сборка TASK-006

Профиль изображений собран на Alpine 3.23 с закреплённым digest базы и libvips `8.17.3-r1`. Компиляция из отдельного build-stage, динамическая линковка libvips. Поставляемая точка входа разрешает JPEG, PNG и WebP; HEIF-плагин не подключён. Инвентарь 85 runtime APK с версиями и заявленными лицензиями для проверенного linux/arm64-образа записан в [CycloneDX SBOM](sbom/image-transform-linux-arm64.cdx.json). У libvips LGPL-2.1-or-later; при распространении образа соблюдать условия динамической линковки, исходников и уведомлений для конкретной сборки. [Пакет Alpine](https://pkgs.alpinelinux.org/package/v3.23/community/aarch64/vips) · [лицензия libvips](https://github.com/libvips/libvips).
