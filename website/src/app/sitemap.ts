/*
  The sitemap.

  It is built from nav.ts rather than by walking the output, so a page that
  exists and is not in the navigation is absent from both — which is the same
  rule the agent-readable build enforces, and the reason there is only one list
  of pages in this project.

  Every entry carries its alternates. The two languages are the same document
  and telling a crawler so is the difference between one page ranking and two
  pages competing with each other for the same query.
*/
import type { MetadataRoute } from 'next';
import { nav, locales } from '@/lib/nav';
import { SITE_URL } from '@/lib/site';

export const dynamic = 'force-static';

export default function sitemap(): MetadataRoute.Sitemap {
  const slugs = nav.flatMap((section) =>
    section.items.map((item) => item.slug),
  );
  const out: MetadataRoute.Sitemap = [];

  const languages = (path: string) =>
    Object.fromEntries(locales.map((l) => [l, `${SITE_URL}/${l}${path}`]));

  for (const locale of locales) {
    // The locale landing page is the entry point and outranks the pages under
    // it, which is what priority is for — not a score, a hint about hierarchy.
    out.push({
      url: `${SITE_URL}/${locale}/`,
      changeFrequency: 'weekly',
      priority: 1,
      alternates: { languages: languages('/') },
    });

    for (const slug of slugs) {
      out.push({
        url: `${SITE_URL}/${locale}/docs/${slug}/`,
        changeFrequency: 'weekly',
        priority: slug === 'introduction' || slug === 'quickstart' ? 0.9 : 0.7,
        alternates: { languages: languages(`/docs/${slug}/`) },
      });
    }
  }

  return out;
}
