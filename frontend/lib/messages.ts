export const messages = {
  ru: {
    title: "AutoLife",
    description: "Автоматизация повседневных задач",
    service: "Состояние API",
    loading: "Проверяется",
    ready: "Готово",
    unavailable: "Временно недоступно",
    refresh: "Проверить снова",
    appearance: "Оформление",
    light: "Светлое",
    dark: "Тёмное",
    system: "Системное",
    error: "Не удалось загрузить страницу",
  },
  en: {
    title: "AutoLife",
    description: "Automation for everyday tasks",
    service: "API status",
    loading: "Checking",
    ready: "Ready",
    unavailable: "Temporarily unavailable",
    refresh: "Check again",
    appearance: "Appearance",
    light: "Light",
    dark: "Dark",
    system: "System",
    error: "The page could not load",
  },
} as const;

export type Locale = keyof typeof messages;
export function isLocale(locale: string): locale is Locale {
  return locale === "ru" || locale === "en";
}
