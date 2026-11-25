"use client";

import { useEffect, useState } from "react";
import { messages, type Locale } from "../lib/messages";

export function ServiceStatus({ locale }: { locale: Locale }) {
  const [status, setStatus] = useState<"loading" | "ready" | "unavailable">("loading");
  const [probe, setProbe] = useState(0);
  const t = messages[locale];

  useEffect(() => {
    const controller = new AbortController();
    const signal = AbortSignal.any([controller.signal, AbortSignal.timeout(5_000)]);
    fetch("/health/ready", { signal, cache: "no-store" })
      .then((response) => response.ok ? response.json() : null)
      .then((result) => setStatus(result?.status === "ready" ? "ready" : "unavailable"))
      .catch(() => {
        if (!controller.signal.aborted) setStatus("unavailable");
      });
    return () => controller.abort();
  }, [probe]);

  return (
    <section aria-labelledby="service-heading" className="surface space-y-3 rounded-xl border p-5">
      <h2 id="service-heading" className="text-lg font-semibold">{t.service}</h2>
      <p role="status">{t[status]}</p>
      <button type="button" onClick={() => { setStatus("loading"); setProbe((n) => n + 1); }}>{t.refresh}</button>
    </section>
  );
}
