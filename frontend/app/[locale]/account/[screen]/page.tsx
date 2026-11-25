import { notFound } from "next/navigation";
import { AccountScreen } from "../../../../components/account";
import { isAccountScreen } from "../../../../lib/account";
import { isLocale } from "../../../../lib/messages";

export const metadata = { title: "AutoLife · Account", referrer: "no-referrer" };

export default async function Page({ params }: { params: Promise<{ locale: string; screen: string }> }) {
  const { locale, screen } = await params;
  if (!isLocale(locale) || !isAccountScreen(screen)) notFound();
  return <AccountScreen locale={locale} screen={screen} />;
}
