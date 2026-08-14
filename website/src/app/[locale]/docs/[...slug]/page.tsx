import Link from "next/link";
import { notFound } from "next/navigation";
import type { Metadata } from "next";
import { getDoc } from "@/lib/content";
import { allSlugs, locales, isLocale, neighbours, t, type Locale } from "@/lib/nav";
import { Sidebar, TableOfContents } from "@/components/Chrome";

export function generateStaticParams() {
  return locales.flatMap((locale) =>
    allSlugs().map((slug) => ({ locale, slug: slug.split("/") })),
  );
}

export async function generateMetadata({
  params,
}: {
  params: Promise<{ locale: string; slug: string[] }>;
}): Promise<Metadata> {
  const { locale: raw, slug } = await params;
  const locale: Locale = isLocale(raw) ? raw : "en";
  const doc = await getDoc(locale, slug.join("/"));
  if (!doc) return {};
  return {
    title: doc.title,
    description: doc.description,
    openGraph: { title: doc.title, description: doc.description, type: "article" },
    alternates: {
      languages: Object.fromEntries(
        locales.map((l) => [l, `/${l}/docs/${slug.join("/")}/`]),
      ),
    },
  };
}

export default async function DocPage({
  params,
}: {
  params: Promise<{ locale: string; slug: string[] }>;
}) {
  const { locale: raw, slug: parts } = await params;
  const locale: Locale = isLocale(raw) ? raw : "en";
  const slug = parts.join("/");
  const doc = await getDoc(locale, slug);
  if (!doc) notFound();

  const { prev, next } = neighbours(slug);

  return (
    <div className="mx-auto flex max-w-[100rem] gap-8 px-4 sm:px-6">
      {/* The sidebar is its own scroll region, so long navigation never drags
          the article with it. */}
      <aside className="sticky top-16 hidden h-[calc(100vh-4rem)] w-64 shrink-0 overflow-y-auto py-8 lg:block">
        <Sidebar locale={locale} />
      </aside>

      <main id="main" className="min-w-0 flex-1 py-8 lg:py-12">
        <article className="glass mx-auto max-w-3xl rounded-2xl px-5 py-8 sm:px-9 sm:py-11">
          <header className="mb-8 border-b border-[var(--rule)] pb-6">
            <h1 className="text-3xl font-bold tracking-tight sm:text-4xl">{doc.title}</h1>
            {doc.description && (
              <p className="mt-3 text-lg leading-relaxed text-[var(--fg-muted)]">
                {doc.description}
              </p>
            )}
          </header>

          <div className="prose" dangerouslySetInnerHTML={{ __html: doc.html }} />

          <nav className="mt-14 grid gap-3 border-t border-[var(--rule)] pt-6 sm:grid-cols-2">
            {prev ? (
              <Link
                href={`/${locale}/docs/${prev.slug}/`}
                className="group rounded-xl border border-[var(--rule)] px-4 py-3 transition hover:border-[var(--accent)]"
              >
                <span className="text-xs text-[var(--fg-faint)]">{t("previous", locale)}</span>
                <span className="mt-0.5 block font-medium text-[var(--accent)]">
                  ← {prev.title[locale]}
                </span>
              </Link>
            ) : (
              <span />
            )}
            {next && (
              <Link
                href={`/${locale}/docs/${next.slug}/`}
                className="group rounded-xl border border-[var(--rule)] px-4 py-3 text-right transition hover:border-[var(--accent)] sm:col-start-2"
              >
                <span className="text-xs text-[var(--fg-faint)]">{t("next", locale)}</span>
                <span className="mt-0.5 block font-medium text-[var(--accent)]">
                  {next.title[locale]} →
                </span>
              </Link>
            )}
          </nav>
        </article>
      </main>

      <aside className="sticky top-16 hidden h-[calc(100vh-4rem)] w-56 shrink-0 overflow-y-auto py-12 xl:block">
        <TableOfContents headings={doc.headings} locale={locale} />
      </aside>
    </div>
  );
}
