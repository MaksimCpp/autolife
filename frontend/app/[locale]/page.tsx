import Link from "next/link";
import { notFound } from "next/navigation";
import { ServiceStatus } from "../../components/service-status";
import { ThemeSelect } from "../../components/theme";
import { accountText } from "../../lib/account";
import { isLocale, messages } from "../../lib/messages";

export default async function Page({ params }: { params: Promise<{ locale: string }> }) {
  const { locale } = await params;
  if (!isLocale(locale)) notFound();
  const t = messages[locale];

  return (
    <main className="mx-auto max-w-2xl space-y-10 px-5 py-12">
      <nav aria-label={locale === "ru" ? "Язык" : "Language"} className="flex flex-wrap items-center gap-4">
        <Link href="/ru" lang="ru" aria-current={locale === "ru" ? "page" : undefined}>Русский</Link>
        <Link href="/en" lang="en" aria-current={locale === "en" ? "page" : undefined}>English</Link>
        <ThemeSelect locale={locale} />
      </nav>
      <header>
        <h1 className="text-3xl font-semibold">{t.title}</h1>
        <p className="mt-2">{t.description}</p>
      </header>
      <nav className="flex flex-wrap gap-4">
        {(["login", "register", "profile"] as const).map((screen) =>
          <Link key={screen} href={`/${locale}/account/${screen}`}>{accountText[locale][screen]}</Link>)}
      </nav>
      <ServiceStatus locale={locale} />
    </main>
  );
}
