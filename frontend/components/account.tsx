"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useTheme } from "next-themes";
import { type FormEvent, type ReactNode, useEffect, useRef, useState } from "react";
import { accountError, accountGET, accountText, accountWrite, AccountError, type AccountScreenName } from "../lib/account";
import type { Locale } from "../lib/messages";

type FormControl = { submit: (work: () => Promise<void>) => Promise<void>; busy: boolean; status: string; error: string; setStatus: (message: string) => void };

function useForm(locale: Locale): FormControl {
  const [busy, setBusy] = useState(false);
  const [status, setStatus] = useState("");
  const [error, setError] = useState("");
  async function submit(work: () => Promise<void>) {
    setBusy(true);
    setStatus("");
    setError("");
    try { await work(); } catch (error) { setError(accountError(error, locale)); } finally { setBusy(false); }
  }
  return { submit, busy, status, error, setStatus };
}

function Feedback({ form }: { form: FormControl }) {
  return (
    <>
      {form.error && <p role="alert" className="account-error">{form.error}</p>}
      {form.status && <p role="status">{form.status}</p>}
    </>
  );
}

function Field({ label, name, type = "text", autoComplete, newPassword = false }: {
  label: string; name: string; type?: string; autoComplete?: string; newPassword?: boolean;
}) {
  return <label className="account-field">{label}<input name={name} type={type} autoComplete={autoComplete}
    minLength={newPassword ? 12 : undefined} maxLength={type === "email" ? 254 : 256} required /></label>;
}

function Panel({ title, children }: { title: string; children: ReactNode }) {
  return <section className="surface space-y-4 rounded-xl border p-5"><h2 className="text-lg font-semibold">{title}</h2>{children}</section>;
}

export function AccountScreen({ locale, screen }: { locale: Locale; screen: AccountScreenName }) {
  const t = accountText[locale];
  const otherLocale = locale === "ru" ? "en" : "ru";
  return (
    <main className="mx-auto max-w-2xl space-y-6 px-5 py-10">
      <nav aria-label={t.language} className="flex flex-wrap gap-4">
        <Link href={`/${locale}`}>AutoLife</Link>
        {screen !== "verify" && screen !== "reset" &&
          <Link href={`/${otherLocale}/account/${screen}`} lang={otherLocale}>{otherLocale === "ru" ? "Русский" : "English"}</Link>}
      </nav>
      <h1 className="text-3xl font-semibold">{t[screen]}</h1>
      {screen === "profile" ? <ProfileSettings locale={locale} /> : <PublicForm locale={locale} screen={screen} />}
    </main>
  );
}

function PublicForm({ locale, screen }: { locale: Locale; screen: Exclude<AccountScreenName, "profile"> }) {
  const t = accountText[locale];
  const router = useRouter();
  const form = useForm(locale);
  const linkToken = useRef("");
  const isToken = screen === "verify" || screen === "reset";
  useEffect(() => {
    if (!isToken || !location.hash) return;
    linkToken.current = new URLSearchParams(location.hash.slice(1)).get("token") ?? "";
    if (location.hash) history.replaceState(history.state, "", location.pathname + location.search);
  }, [isToken]);

  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const target = event.currentTarget;
    const fields = new FormData(target);
    void form.submit(async () => {
      if (isToken && !linkToken.current) {
        form.setStatus(t.missingToken);
        return;
      }
      if (screen === "login" || screen === "register") {
        await accountWrite(`/auth/${screen}`, "POST", { email: fields.get("email"), password: fields.get("password") }, true);
        if (screen === "login") router.replace(`/${locale}/account/profile`);
        else form.setStatus(t.registerSent);
      } else if (screen === "forgot") {
        await accountWrite("/auth/password/forgot", "POST", { email: fields.get("email") }, true);
        form.setStatus(t.resetSent);
      } else if (screen === "verify") {
        await accountWrite("/auth/verify", "POST", { token: linkToken.current }, true);
        linkToken.current = "";
        form.setStatus(t.verified);
      } else {
        await accountWrite("/auth/password/reset", "POST", { token: linkToken.current, new_password: fields.get("password") }, true);
        linkToken.current = "";
        form.setStatus(t.resetDone);
      }
      for (const field of target.querySelectorAll<HTMLInputElement>('input[type="password"]')) field.value = "";
    });
  }

  return (
    <>
      <form className="account-form surface rounded-xl border p-5" onSubmit={onSubmit}>
        {!isToken && <Field label={t.email} name="email" autoComplete="email" type="email" />}
        {screen !== "verify" && screen !== "forgot" &&
          <Field label={screen === "reset" ? t.newPassword : t.password} name="password" type="password"
            autoComplete={screen === "login" ? "current-password" : "new-password"} newPassword={screen !== "login"} />}
        {(screen === "register" || screen === "reset") && <p className="text-sm">{t.passwordHint}</p>}
        <button disabled={form.busy}>{form.busy ? t.working : t[screen]}</button>
        <Feedback form={form} />
      </form>
      <nav className="flex flex-wrap gap-4">
        {(["login", "register", "forgot"] as const).filter((link) => link !== screen).map((link) =>
          <Link href={`/${locale}/account/${link}`} key={link}>{t[link]}</Link>)}
      </nav>
    </>
  );
}

type Settings = { id: string; email: string; locale: Locale; timezone: string; theme: "light" | "dark" | "system"; notifications: boolean; analytics_consent: boolean };
type Session = { id: string; created_at: string; expires_at: string; current: boolean };
type SessionPage = { items: Session[]; next_cursor?: string };
type ExportJob = { id: string; state: "requested" | "running" | "ready" | "failed" | "expired" };

function ProfileSettings({ locale }: { locale: Locale }) {
  const t = accountText[locale];
  const router = useRouter();
  const { setTheme } = useTheme();
  const form = useForm(locale);
  const [profile, setProfile] = useState<Settings | null>(null);
  const [sessions, setSessions] = useState<SessionPage>({ items: [] });
  const [loadError, setLoadError] = useState("");
  const [exportJob, setExportJob] = useState<ExportJob | null>(null);
  const [deleted, setDeleted] = useState(false);
  const exportKey = useRef("");
  useEffect(() => {
    const controller = new AbortController();
    Promise.all([accountGET<Settings>("/me", controller.signal), accountGET<SessionPage>("/me/sessions", controller.signal)])
      .then(([profile, sessions]) => {
        setProfile(profile);
        setSessions(sessions);
        setTheme(profile.theme);
        const id = sessionStorage.getItem("autolife-export:" + profile.id);
        if (id) setExportJob({ id, state: "requested" });
      })
      .catch((error) => {
        if (controller.signal.aborted) return;
        if (error instanceof AccountError && error.status === 401) router.replace(`/${locale}/account/login`);
        else setLoadError(accountError(error, locale));
      });
    return () => controller.abort();
  }, [locale, router, setTheme]);
  useEffect(() => {
    if (!exportJob || (exportJob.state !== "requested" && exportJob.state !== "running")) return;
    const { id } = exportJob;
    const controller = new AbortController();
    const timer = setTimeout(() => {
      accountGET<ExportJob>("/me/exports/" + id, controller.signal)
        .then(setExportJob).catch((error) => { if (!controller.signal.aborted) setLoadError(accountError(error, locale)); });
    }, 1000);
    return () => { clearTimeout(timer); controller.abort(); };
  }, [exportJob, locale]);

  function submit(event: FormEvent<HTMLFormElement>, work: (data: FormData) => Promise<void>) {
    event.preventDefault();
    const target = event.currentTarget;
    const data = new FormData(target);
    void form.submit(async () => {
      await work(data);
      for (const field of target.querySelectorAll<HTMLInputElement>('input[type="password"]')) field.value = "";
    });
  }
  async function save(fields: FormData) {
    const next = await accountWrite<Settings>("/me", "PATCH", {
      locale: fields.get("locale"), timezone: fields.get("timezone"), theme: fields.get("theme"),
      notifications: fields.has("notifications"), analytics_consent: fields.has("consent"),
    });
    setProfile(next);
    setTheme(next.theme);
    if (next.locale !== locale) router.replace(`/${next.locale}/account/profile`);
    else form.setStatus(t.saved);
  }
  async function logout() {
    await accountWrite("/auth/logout", "POST");
    router.replace(`/${locale}/account/login`);
  }
  async function revoke(session: Session) {
    await accountWrite("/me/sessions/" + session.id, "DELETE");
    if (session.current) router.replace(`/${locale}/account/login`);
    else setSessions(await accountGET<SessionPage>("/me/sessions"));
  }
  async function moreSessions() {
    const next = await accountGET<SessionPage>("/me/sessions?cursor=" + encodeURIComponent(sessions.next_cursor ?? ""));
    setSessions({ ...next, items: [...sessions.items, ...next.items] });
  }
  async function change(fields: FormData) {
    await accountWrite("/auth/password/change", "POST", {
      current_password: fields.get("current_password"), new_password: fields.get("new_password"),
    });
    setSessions(await accountGET<SessionPage>("/me/sessions"));
    form.setStatus(t.changed);
  }
  async function requestExport(fields: FormData) {
    exportKey.current ||= crypto.randomUUID();
    const job = await accountWrite<{ id: string }>("/me/export", "POST", { current_password: fields.get("current_password") }, false, exportKey.current);
    exportKey.current = "";
    if (profile) sessionStorage.setItem("autolife-export:" + profile.id, job.id);
    setExportJob({ ...job, state: "requested" });
  }
  async function download() {
    if (!exportJob) return;
    const data = await accountWrite<{ url: string }>(`/me/exports/${exportJob.id}/download`, "POST");
    if (data.url.startsWith("/api/v1/me/exports/")) window.location.assign(data.url);
  }
  async function deleteAccount(fields: FormData) {
    await accountWrite("/me", "DELETE", { current_password: fields.get("current_password"), confirm: fields.has("confirm") });
    if (profile) sessionStorage.removeItem("autolife-export:" + profile.id);
    setDeleted(true);
    form.setStatus(t.deleted);
  }

  if (deleted) return <Feedback form={form} />;
  if (!profile) return loadError ? <p role="alert">{loadError}</p> : <p role="status">{t.working}</p>;

  return (
    <div className="space-y-5">
      <Feedback form={form} />
      {loadError && <p role="alert">{loadError}</p>}
      <form className="account-form surface rounded-xl border p-5" onSubmit={(event) => submit(event, save)}>
        <p>{t.email}: {profile.email}</p>
        <label className="account-field">{t.language}<select name="locale" defaultValue={profile.locale}><option value="ru">Русский</option><option value="en">English</option></select></label>
        <label className="account-field">{t.timezone}<input name="timezone" defaultValue={profile.timezone} maxLength={128} required /></label>
        <label className="account-field">{t.theme}<select name="theme" defaultValue={profile.theme}>
          <option value="system">{t.system}</option><option value="light">{t.light}</option><option value="dark">{t.dark}</option>
        </select></label>
        <label><input type="checkbox" name="notifications" defaultChecked={profile.notifications} /> {t.notifications}</label>
        <label><input type="checkbox" name="consent" defaultChecked={profile.analytics_consent} /> {t.consent}</label>
        <button disabled={form.busy}>{t.save}</button>
      </form>
      <Panel title={t.sessions}>
        <ul className="space-y-2">{sessions.items.map((session) => <li key={session.id} className="flex flex-wrap items-center justify-between gap-3">
          <span>{session.current ? t.current : new Date(session.created_at).toLocaleString(locale)}</span>
          <button disabled={form.busy} onClick={() => void form.submit(() => revoke(session))}>{t.revoke}</button>
        </li>)}</ul>
        {sessions.next_cursor && <button disabled={form.busy} onClick={() => void form.submit(moreSessions)}>{t.more}</button>}
        <button disabled={form.busy} onClick={() => void form.submit(logout)}>{t.logout}</button>
      </Panel>
      <Panel title={t.change}>
        <form className="account-form" onSubmit={(event) => submit(event, change)}>
          <Field label={t.currentPassword} name="current_password" type="password" autoComplete="current-password" />
          <Field label={t.newPassword} name="new_password" type="password" autoComplete="new-password" newPassword />
          <button disabled={form.busy}>{t.change}</button>
        </form>
      </Panel>
      <Panel title={t.privacy}>
        <form className="account-form" onSubmit={(event) => submit(event, requestExport)}>
          <Field label={t.currentPassword} name="current_password" type="password" autoComplete="current-password" />
          <button disabled={form.busy}>{t.export}</button>
        </form>
        {exportJob && <p role="status">{t.exportStatus}: {t[exportJob.state]}</p>}
        {exportJob?.state === "ready" && <button disabled={form.busy} onClick={() => void form.submit(download)}>{t.download}</button>}
        <form className="account-form" onSubmit={(event) => submit(event, deleteAccount)}>
          <Field label={t.currentPassword} name="current_password" type="password" autoComplete="current-password" />
          <label><input name="confirm" type="checkbox" required /> {t.confirmDelete}</label>
          <button disabled={form.busy} className="account-danger">{t.delete}</button>
        </form>
      </Panel>
    </div>
  );
}
