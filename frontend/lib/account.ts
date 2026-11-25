import type { Locale } from "./messages";

export const accountText = {
  en: {
    login: "Sign in", register: "Create account", verify: "Verify email", forgot: "Recover access", reset: "Reset password",
    profile: "Account settings", email: "Email", password: "Password", currentPassword: "Current password",
    newPassword: "New password", passwordHint: "Use at least 12 characters.", working: "Working…",
    registerSent: "If registration can proceed, a verification link will arrive by email.",
    resetSent: "If the account can be recovered, you’ll receive a link by email.",
    verified: "Email verified. You can sign in now.", resetDone: "Password updated. Sign in again.",
    missingToken: "Open the link from your email to continue.", error: "Something went wrong. Try again.",
    invalid: "Check the information you entered.", unauthorized: "Check your password or sign in again.",
    unverified: "Verify your email before signing in.", forbidden: "Refresh the page and try again.",
    rate: "Too many attempts. Try again later.", unavailable: "The service is temporarily unavailable.",
    language: "Language", timezone: "Time zone", theme: "Theme", light: "Light", dark: "Dark", system: "System",
    notifications: "Receive notifications", consent: "Allow optional usage analytics", save: "Save settings", saved: "Settings saved.",
    sessions: "Active sessions", current: "Current session", revoke: "Revoke session", more: "More sessions",
    logout: "Sign out", change: "Change password", changed: "Password changed.", privacy: "Your data", export: "Request export",
    exportStatus: "Export status", download: "Download export", delete: "Delete account",
    confirmDelete: "I confirm that I want to delete my account.", deleted: "Deletion accepted. Your sessions have been revoked.",
    requested: "Requested", running: "Processing", ready: "Ready", failed: "Failed", expired: "Expired",
  },
  ru: {
    login: "Войти", register: "Создать аккаунт", verify: "Подтвердить почту", forgot: "Восстановить доступ", reset: "Сбросить пароль",
    profile: "Настройки аккаунта", email: "Электронная почта", password: "Пароль", currentPassword: "Текущий пароль",
    newPassword: "Новый пароль", passwordHint: "Не менее 12 символов.", working: "Выполняется…",
    registerSent: "Если регистрация возможна, на почту придёт ссылка для подтверждения.",
    resetSent: "Если доступ можно восстановить, на почту придёт ссылка.",
    verified: "Почта подтверждена. Теперь можно войти.", resetDone: "Пароль обновлён. Войдите снова.",
    missingToken: "Откройте ссылку из письма, чтобы продолжить.", error: "Произошла ошибка. Попробуйте снова.",
    invalid: "Проверьте введённые данные.", unauthorized: "Проверьте пароль или войдите снова.",
    unverified: "Подтвердите почту перед входом.", forbidden: "Обновите страницу и попробуйте снова.",
    rate: "Слишком много попыток. Попробуйте позже.", unavailable: "Сервис временно недоступен.",
    language: "Язык", timezone: "Часовой пояс", theme: "Тема", light: "Светлая", dark: "Тёмная", system: "Системная",
    notifications: "Получать уведомления", consent: "Разрешить необязательную аналитику использования", save: "Сохранить настройки", saved: "Настройки сохранены.",
    sessions: "Активные сессии", current: "Текущая сессия", revoke: "Отозвать сессию", more: "Ещё сессии",
    logout: "Выйти", change: "Изменить пароль", changed: "Пароль изменён.", privacy: "Ваши данные", export: "Запросить экспорт",
    exportStatus: "Статус экспорта", download: "Скачать экспорт", delete: "Удалить аккаунт",
    confirmDelete: "Подтверждаю удаление аккаунта.", deleted: "Запрос на удаление принят. Ваши сессии отозваны.",
    requested: "Запрошен", running: "Обрабатывается", ready: "Готов", failed: "Ошибка", expired: "Истёк",
  },
} satisfies Record<Locale, Record<string, string>>;

export class AccountError extends Error {
  constructor(readonly code: string, readonly status: number) { super(code); }
}

async function readJSON<T>(response: Response): Promise<T> {
  if (!response.ok) {
    const data = await response.json().catch(() => null);
    throw new AccountError(data?.error?.code ?? "", response.status);
  }
  if (response.status === 204 || response.headers.get("content-type")?.includes("application/json") !== true) {
    return undefined as T;
  }
  return response.json();
}

export async function accountGET<T>(path: string, signal?: AbortSignal): Promise<T> {
  return readJSON<T>(await fetch("/api/v1" + path, { signal, cache: "no-store" }));
}

export async function accountWrite<T>(path: string, method: string, body?: unknown, pre = false, key?: string): Promise<T> {
  const csrf = await accountGET<{ csrf_token: string }>("/auth/csrf" + (pre ? "?pre=1" : ""));
  const headers = new Headers({ "X-CSRF-Token": csrf.csrf_token });
  if (body !== undefined) headers.set("Content-Type", "application/json");
  if (key) headers.set("Idempotency-Key", key);
  return readJSON<T>(await fetch("/api/v1" + path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) }));
}

export function accountError(error: unknown, locale: Locale): string {
  const t = accountText[locale];
  if (!(error instanceof AccountError)) return t.error;
  if (error.code === "EMAIL_NOT_VERIFIED") return t.unverified;
  const messages: Record<number, string> = {
    400: t.invalid, 401: t.unauthorized, 403: t.forbidden, 422: t.invalid, 429: t.rate, 503: t.unavailable,
  };
  return messages[error.status] ?? t.error;
}

const screens = ["login", "register", "verify", "forgot", "reset", "profile"] as const;
export type AccountScreenName = (typeof screens)[number];

export function isAccountScreen(value: string): value is AccountScreenName {
  return screens.some((screen) => screen === value);
}
