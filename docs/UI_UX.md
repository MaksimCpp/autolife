# UI/UX — AutoLife

## Design direction
Современный минималистичный SaaS без перегруженного технического интерфейса. Тёмная тема — основная визуальная концепция; светлая полноценная; выбор dark/light/system сохраняется, системная настройка учитывается. Не смешивать акценты на каждой карточке; использовать цвет для смысла, не только цвет для статуса. Мягкие градиенты лишь на landing/акценте. Базовые токены (WCAG 2.2 AA):

| Token | Dark | Light |
|---|---|---|
| background | #0B1120 | #F1F5F9 |
| surface | #141E30 | #FFFFFF |
| text | #F1F5F9 | #0F172A |
| muted text | #A7B6CA | #475569 |
| декоративный border | #334155 | #CBD5E1 |
| граница контрола / focus | #94A3B8 | #475569 |
| primary button | #2563EB | #1D4ED8 |
| secondary accent | #38BDF8 | #0369A1 |
| teal | #2DD4BF | #0F766E |
| emerald/success | #34D399 | #047857 |
| error | #FCA5A5 | #B91C1C |
| warning | #FBBF24 | #92400E |

Шрифт Inter или системный с кириллицей; 14–16px body, заголовки 20–32px, spacing 4/8/12/16/24/32, radius 8–12px, Lucide icons. Белый текст на primary. Значимые границы и focus используют отдельный токен, с контрастом ≥3:1 к соседней поверхности. На primary проверять контраст состояния фокуса отдельно. Видимый focus, aria labels, клавиатура, reduced motion, доступные формы, контраст текста 4.5:1 для обычного текста. Обычные страницы должны поддерживать reflow от 320 CSS px и масштабирование. Canvas имеет доступный списочный эквивалент. Не использовать исключительно drag-and-drop: кнопки вверх/вниз и доступные формы.

## Навигация и страницы
Public landing: ценность, примеры шаблонов, CTA, privacy/terms. Auth: простые формы с понятными ошибками. Desktop sidebar: Overview, Automations, Templates, Files, History, Settings; будущие доступные модули добавляются по feature availability. Mobile: компактное меню, без горизонтального скролла обычных страниц. Не показывать неработающие кнопки будущих модулей. Overview: создать сценарий, популярные шаблоны, последние запуски, usage и статистика. Files: загрузка, список, формат/размер/TTL, download/delete. History: фильтры, status, elapsed, details, retry/cancel. Settings: profile, language, theme, privacy, data export/delete, sessions.

## Workflow editor
Desktop: слева каталог действий, центр React Flow canvas, справа параметры выбранного шага; toolbar имя, save, validate, test/run. MVP линейная цепочка с единым порядком шагов для canvas и списочного редактора, подсказки вход/выход, визуальные связи и порядок. Mobile: пошаговый список с редактированием вместо обязательного canvas. Черновик допускает ошибки, запуск запрещён до валидации. Показать input file picker, лимиты, результаты и срок хранения. Templates: мастер 1) выбрать 2) параметры 3) загрузка 4) запуск. Пользователь не обязан понимать JSON или API.

## Будущие экраны
Watch: URL, выбор элемента/условия, baseline, расписание, preview и история. AutoInbox: подключение с описанием scopes, фильтры, sample preview, сводки. LifePilot: календарь, обязательства, подтверждение извлечённых полей. AI: текстовый запрос, черновик, необходимые разрешения, явное подтверждение. Analytics: период, counts, trend, success rate с denominator, estimated saved time с методикой. Admin: отдельная защищённая зона.

## Состояния и приёмка
Empty state с полезным CTA; skeleton, loading, error+retry, offline, expired artifact, quota exceeded, permission revoked. Не рисовать фиктивный процент прогресса: шаги или неопределённый indicator. Confirm irreversible actions. RU/EN strings, локальные числа/даты. Проверить обе темы, keyboard, screen reader, 360/768/1440 widths, отсутствие overflow, ошибки и empty states. Визуальные макеты пока не приложены; дизайн-токены предварительные, референсы и скриншоты редактора, мастера и состояний подготовить в TASK-008. Базовые токены и RU/EN используются с каркаса TASK-001.

Тестовый запуск показывает revision и создаёт immutable snapshot без смены active_version. История обеспечивает возврат после закрытия вкладки, включая expired и retry с заменой входов. Проверить 320/360/768/1440 px, zoom, длинные RU/EN строки, доступные имена контролов и объявления статусов.
