# API — REST v1

Base `/api/v1`. JSON UTF-8; file upload streaming multipart через API, один файл на запрос; cookie auth same origin, CSRF token и проверка Origin для cookie-auth мутаций. Server obtains owner from session. UUID IDs, ISO timestamps, cursor pagination `limit`/`cursor` (default 20, max 100). Error: `{"error":{"code":"VALIDATION_ERROR","message":"...","fields":[],"request_id":"..."}}`. 400 invalid syntax, 401 unauthenticated, 403 forbidden permission, 404 inaccessible private resource, 409 conflict/idempotency mismatch, 413 size, 415 type, 422 invalid config, 429 quota/rate, 503 unavailable. Never leak internal exception. OpenAPI 3.1 проверяется до реализации каждого endpoint.

## MVP endpoints
| Method | Path | Contract |
|---|---|---|
| POST | /auth/register | email,password → 202 generic response |
| POST | /auth/verify | token → 204 |
| POST | /auth/login | email,password → session cookie, user |
| POST | /auth/logout | revoke current → 204 |
| POST | /auth/password/forgot | email → generic 202 |
| POST | /auth/password/reset | token,new_password → 204 |
| POST | /auth/password/change | текущий и новый пароль → 204, ротация текущей и отзыв остальных сессий |
| GET | /auth/csrf | токен для текущей сессии либо короткоживущего pre-auth контекста |
| GET/PATCH | /me | own profile/settings |
| GET | /me/sessions | список своих сессий без credentials |
| DELETE | /me/sessions/{id} | отзыв своей сессии → 204 |
| POST | /me/export | недавняя авторизация, durable job → 202 + ID |
| GET | /me/exports/{id} | статус и срок, download action доступен в ready |
| POST | /me/exports/{id}/download | авторизованная выдача краткосрочной ссылки на готовый экспорт |
| DELETE | /me | недавняя авторизация и явное подтверждение → 202 после durable job и подтверждённой записи в журнале restore, отзыв сессий |
| GET/POST | /files | list / upload → file metadata |
| GET/DELETE | /files/{id} | metadata / delete, active pin conflict 409 |
| POST | /files/{id}/download | scoped short-lived download URL |
| GET | /actions | available type/version and schemas |
| GET | /triggers | available type/version and schemas |
| GET/POST | /workflows | list / create draft |
| GET/PATCH/DELETE | /workflows/{id} | read / update / archive |
| PUT | /workflows/{id}/draft | definition + expected revision |
| POST | /workflows/{id}/validate | errors and warnings |
| POST | /workflows/{id}/versions | expected_revision → immutable version и замена active_version в одной транзакции |
| GET | /workflows/{id}/versions | список версий с cursor, исключая внутренние снимки |
| POST | /workflows/{id}/test-executions | expected_revision + input refs → 202, сохраняет immutable snapshot, active_version прежний |
| POST | /workflows/{id}/copy | independent draft |
| POST | /workflows/{id}/executions | version_id,input refs → 202 execution ID |
| GET | /executions | list, state/workflow filter |
| GET | /executions/{id} | status, steps, timestamps, output refs, safe errors |
| POST | /executions/{id}/cancel | 202 / current terminal state |
| POST | /executions/{id}/retry | new execution, historical version, required inputs |
| GET | /templates | search/category |
| POST | /templates/{id}/instantiate | new independent workflow |
| GET | /usage | limits and actual/reserved usage |
| GET | /stats | own aggregate metrics, date range |
| GET | /notifications | список с cursor |
| PATCH | /notifications/{id} | пометить своё уведомление прочитанным, операция идемпотентна |

## Общие правила контрактов

Создание execution, test-execution, retry и export требует Idempotency-Key. Область — owner и операция, hash включает целевой ресурс и канонический payload с сохранением порядка массивов. Окно повтора 24 часа с первого commit. Повтор с тем же payload возвращает исходный статус принятия и ID, другой payload даёт 409. Уникальность и результат фиксируются в транзакции создания. При истечении окна отправка может создать новую операцию. Конфликты draft revision, состояния и idempotency возвращают разные machine codes. Поля ошибок включают code, path и при необходимости step_id. Cursor непрозрачный и привязан к сортировке и фильтру, tie-breaker ID при одинаковом created_at.

Неподтверждённый пользователь получает подходящий для формы безопасный ответ. Pre-auth формы проходят проверку Origin и CSRF в короткоживущем pre-auth контексте. Токены verify/reset одноразовые, их значения не попадают в URL журналов и аналитику. Reset отзывает все сессии. Смена пароля, экспорт и удаление требуют недавней проверки credentials. После принятия удаления статус отслеживается внутри durable job, публичный lookup отсутствует.

Upload использует staging, потоковые лимиты без доверия к Content-Length, проверку формата/размера и commit ready после валидации. Клиент сохраняет порядок своих входов отдельным массивом. Download выдаёт приватную ссылку сроком до 5 минут и не дольше оставшегося TTL. Уже выданная ссылка действует до истечения либо удаления объекта. Истёкшие результаты остаются в истории как expired. Cancel 202 означает приём запроса. Для terminal state возвращается 200 со статусом. Retry требует доступных входов или replacements и запрещён у архивного workflow. Версии исполнения доступны через описание execution, включая происхождение из тестового снимка. Контракт лимитов следует REQUIREMENTS.

## Future groups
/monitors, /monitors/{id}/checks, /connections, /oauth/{provider}/callback, /inbox/sources, /obligations, /reminders, /approvals, /ai/workflow-drafts, /billing, /webhooks/{provider}, /admin/analytics. Detailed contracts before respective stage. OAuth callback and webhook special auth, not ordinary session mutation. Health /health/live and /health/ready, /metrics private. No unrestricted debug endpoints. E2E contract tests and API version compatibility required.

Начальная машиночитаемая спецификация здоровья API находится в [OpenAPI 3.1](openapi.yaml). Пути /health/live и /health/ready доступны на том же origin, что и frontend.

## Аккаунты TASK-002

Все аккаунтные маршруты описаны в [OpenAPI 3.1](openapi.yaml). `GET /auth/csrf?pre=1` выдаёт pre-auth контекст для публичных форм даже при наличии рабочей сессии. Мутации требуют точный `Origin` и заголовок `X-CSRF-Token`. Pre-auth контекст действует 15 минут, сессия семь суток. Пароль содержит не менее 12 символов и не более 256 байт UTF-8.

`POST /me/export` принимает `Idempotency-Key`, `DELETE /me` принимает `confirm: true`. Операции принимают `current_password` для повторной проверки. Если пароль не передан, требуется сессия, подтверждённая в последние 10 минут. Выдача экспорта требует живой сессии владельца, а выданная ссылка привязана к этой сессии. Повтор DELETE с тем же токеном и CSRF подтверждает исходный запрос в пределах 24 часов. После принятия токен теряет доступ к остальным маршрутам.
