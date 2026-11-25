"use client";

import { ThemeProvider, useTheme } from "next-themes";
import { useSyncExternalStore, type ReactNode } from "react";
import { messages, type Locale } from "../lib/messages";

const subscribe = () => () => {};

export function Theme({ children }: { children: ReactNode }) {
  return <ThemeProvider attribute="data-theme" defaultTheme="system">{children}</ThemeProvider>;
}

export function ThemeSelect({ locale }: { locale: Locale }) {
  const { theme, setTheme } = useTheme();
  const hydrated = useSyncExternalStore(subscribe, () => true, () => false);
  const t = messages[locale];

  return (
    <label className="flex flex-wrap items-center gap-2">
      {t.appearance}
      <select value={hydrated ? (theme ?? "system") : "system"} disabled={!hydrated} onChange={(event) => setTheme(event.target.value)}>
        <option value="system">{t.system}</option>
        <option value="light">{t.light}</option>
        <option value="dark">{t.dark}</option>
      </select>
    </label>
  );
}
