/*
  robots.txt.

  Everything here is public documentation and all of it should be indexed, so
  the interesting content is not the disallow list — it is the sitemap pointer,
  which is how a crawler finds the pages no link happens to reach.

  The agent-readable files are deliberately allowed. They are the same
  documentation in plain text, and a crawler that prefers them to the rendered
  page is getting a better answer, not a leak.
*/
import type { MetadataRoute } from 'next';
import { SITE_URL } from '@/lib/site';

export const dynamic = 'force-static';

export default function robots(): MetadataRoute.Robots {
  return {
    rules: [
      {
        userAgent: '*',
        allow: '/',
        // The search index is a build artifact the browser fetches; it is not a
        // page and a crawler indexing it would be indexing JSON.
        disallow: ['/search-en.json', '/search-ru.json'],
      },
    ],
    sitemap: `${SITE_URL}/sitemap.xml`,
    host: SITE_URL,
  };
}
