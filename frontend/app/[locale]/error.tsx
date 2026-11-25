"use client";

import { useParams } from "next/navigation";
import { isLocale, messages } from "../../lib/messages";

export default function ErrorPage({ reset }: { reset: () => void }) {
  const { locale } = useParams<{ locale: string }>();
  const t = messages[isLocale(locale) ? locale : "ru"];

  return (
    <main className="mx-auto max-w-2xl space-y-4 px-5 py-12">
      <h1 className="text-2xl font-semibold">{t.error}</h1>
      <button type="button" onClick={reset}>{t.refresh}</button>
    </main>
  );
}
