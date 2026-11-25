# Integrations — внешние подключения

## Приоритеты
P1: Telegram (бот: добровольно полученные файлы/сообщения, уведомления), Gmail (read scopes), Google Drive (файлы), Sheets (таблицы), Calendar (события). P2: Outlook, OneDrive, Dropbox, IMAP/SMTP, webhook, HTTP API. В MVP только служебный email; пользовательские интеграции не обязательны.

## Контракты
Provider registry: ID, версии, разрешённые capabilities, auth method, scopes, actions/triggers. Connection: owner, provider, account label, granted scopes, encrypted credential ref, expiry, status. Connection IDs в сценарии вместо токенов. Проверять владельца, capability и текущее разрешение непосредственно перед каждым внешним действием. OAuth authorization code + PKCE где поддерживается, state одноразовый с TTL и связью с сессией, точный redirect URI. Токены шифруются с отдельным ключом и ротацией; refresh конкурентно безопасен. Не выдавать их UI. Disconnect блокирует новые запросы, отзывает доступ где возможно и удаляет секрет.

## Надёжность и безопасность
Timeout, provider-specific rate limits, Retry-After, backoff, ограниченные retries; idempotency/reconciliation для внешних записей. UNKNOWN_OUTCOME без слепого повторения. Webhook: проверка подписи/секрета, replay window, event dedup; внешние HTTP-запросы через SSRF-guard и egress policy. Не поддерживать пользовательский произвольный код. Изоляция провайдеров; no secrets in logs. До запуска проверить актуальные правила, OAuth verification, scopes, квоты и стоимость каждого провайдера.

## Приёмка
Mock provider проверяет connect, invalid state, expiry/refresh race, scope denial, revoke, retry и duplicate callback. Отозванное подключение не работает для новых шагов. Никаких production credentials в тестах. Статистика: integration.connection_created, integration.connection_revoked, integration.action_completed, latency/cost без токенов/адресов и содержимого.

В TASK-012 read и настройка capability проверяются до включения write. Публичная запись требует проверки revoke, безопасного повтора и UNKNOWN_OUTCOME в 012.05. P1 допускает явно подтверждённую при настройке политику ограниченных эффектов. Общий durable approval реализуется в 013.05. Доступность и согласие владельца аккаунта проверяются до подключения реальных сервисов.
