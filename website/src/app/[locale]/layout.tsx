import Link from "next/link";
import { locales, isLocale, t, type Locale } from "@/lib/nav";
import { GopherMark } from "@/components/Gopher";
import { LanguageSwitch, MobileNav, ThemeToggle, CodeCopy } from "@/components/Chrome";
import { SearchButton } from "@/components/Search";

export function generateStaticParams() {
  return locales.map((locale) => ({ locale }));
}

export default async function LocaleLayout({
  children,
  params,
}: {
  children: React.ReactNode;
  params: Promise<{ locale: string }>;
}) {
  const { locale: raw } = await params;
  const locale: Locale = isLocale(raw) ? raw : "en";

  return (
    <div lang={locale}>
      <a
        href="#main"
        className="sr-only focus:not-sr-only focus:absolute focus:left-4 focus:top-4 focus:z-[200] focus:rounded-lg focus:bg-[var(--glass-strong)] focus:px-4 focus:py-2"
      >
        {locale === "ru" ? "К содержимому" : "Skip to content"}
      </a>

      <header className="sticky top-0 z-40 border-b border-[var(--rule)] backdrop-blur-xl">
        <div className="absolute inset-0 -z-10 bg-[var(--glass)]" />
        <div className="mx-auto flex h-16 max-w-[100rem] items-center gap-3 px-4 sm:px-6">
          <MobileNav locale={locale} />

          <Link
            href={`/${locale}/`}
            className="flex shrink-0 items-center gap-2.5 font-semibold tracking-tight"
          >
            <GopherMark className="h-8 w-8" />
            <span className="text-lg">orm</span>
            <span className="hidden text-xs font-normal text-[var(--fg-faint)] sm:inline">
              for PostgreSQL
            </span>
          </Link>

          <nav className="ml-4 hidden items-center gap-1 md:flex">
            <Link
              href={`/${locale}/docs/introduction/`}
              className="rounded-lg px-3 py-1.5 text-sm text-[var(--fg-muted)] transition hover:bg-[var(--accent-soft)] hover:text-[var(--fg)]"
            >
              {t("docs", locale)}
            </Link>
            <Link
              href={`/${locale}/docs/cookbook/queries/`}
              className="rounded-lg px-3 py-1.5 text-sm text-[var(--fg-muted)] transition hover:bg-[var(--accent-soft)] hover:text-[var(--fg)]"
            >
              {locale === "ru" ? "Рецепты" : "Cookbook"}
            </Link>
          </nav>

          <div className="ml-auto flex items-center gap-2">
            <SearchButton locale={locale} />
            <LanguageSwitch locale={locale} />
            <ThemeToggle locale={locale} />
            <a
              href="https://github.com/AlexAli29/orm"
              target="_blank"
              rel="noreferrer noopener"
              aria-label="GitHub"
              className="hidden h-9 w-9 place-items-center rounded-xl border border-[var(--rule)] bg-[var(--glass)] text-[var(--fg-muted)] transition hover:text-[var(--accent)] sm:grid"
            >
              <svg width="17" height="17" viewBox="0 0 24 24" fill="currentColor">
                <path d="M12 .5C5.7.5.5 5.7.5 12c0 5.1 3.3 9.4 7.9 10.9.6.1.8-.2.8-.6v-2c-3.2.7-3.9-1.5-3.9-1.5-.5-1.3-1.3-1.7-1.3-1.7-1-.7.1-.7.1-.7 1.1.1 1.7 1.1 1.7 1.1 1 1.8 2.7 1.3 3.4 1 .1-.7.4-1.3.7-1.6-2.6-.3-5.3-1.3-5.3-5.8 0-1.3.5-2.3 1.2-3.1-.1-.3-.5-1.5.1-3.1 0 0 1-.3 3.2 1.2.9-.3 1.9-.4 2.9-.4s2 .1 2.9.4c2.2-1.5 3.2-1.2 3.2-1.2.6 1.6.2 2.8.1 3.1.8.8 1.2 1.8 1.2 3.1 0 4.5-2.7 5.5-5.3 5.8.4.4.8 1.1.8 2.2v3.3c0 .3.2.7.8.6 4.6-1.5 7.9-5.8 7.9-10.9C23.5 5.7 18.3.5 12 .5Z" />
              </svg>
            </a>
          </div>
        </div>
      </header>

      {children}
      <CodeCopy locale={locale} />
    </div>
  );
}
