/*
  JSON-LD.

  It is here because the machines that read this site are not only search
  engines any more, and a page that states what it is in a form they already
  parse is cheaper to understand than one they have to infer from headings.

  Nothing here is a claim the page does not already make in prose. A structured
  description that says something the page does not is the SEO equivalent of a
  comment that lies.
*/
import { SITE_URL } from '@/lib/site';

function Script({ data }: { data: unknown }) {
  return (
    <script
      type="application/ld+json"
      // The content is built here from our own strings, not from user input.
      dangerouslySetInnerHTML={{ __html: JSON.stringify(data) }}
    />
  );
}

/** The library itself, for the landing page. */
export function SoftwareSchema({ locale }: { locale: string }) {
  return (
    <Script
      data={{
        '@context': 'https://schema.org',
        '@type': 'SoftwareSourceCode',
        name: 'orm',
        description:
          'A PostgreSQL-native data mapper for Go that reconciles user-owned structs against the real database schema and generates a type-safe query API from what it proved.',
        url: `${SITE_URL}/${locale}/`,
        codeRepository: 'https://github.com/AlexAli29/orm',
        programmingLanguage: { '@type': 'ComputerLanguage', name: 'Go' },
        runtimePlatform: 'Go 1.24',
        license: 'https://opensource.org/licenses/MIT',
        author: {
          '@type': 'Person',
          name: 'AlexAli29',
          url: 'https://github.com/AlexAli29',
        },
      }}
    />
  );
}

/** One documentation page, and the trail that leads to it. */
export function ArticleSchema({
  locale,
  slug,
  title,
  description,
  section,
}: {
  locale: string;
  slug: string;
  title: string;
  description?: string;
  section?: string;
}) {
  const url = `${SITE_URL}/${locale}/docs/${slug}/`;
  return (
    <>
      <Script
        data={{
          '@context': 'https://schema.org',
          '@type': 'TechArticle',
          headline: title,
          description,
          url,
          inLanguage: locale === 'ru' ? 'ru-RU' : 'en-US',
          isPartOf: {
            '@type': 'WebSite',
            name: 'orm documentation',
            url: SITE_URL,
          },
          author: { '@type': 'Person', name: 'AlexAli29' },
          proficiencyLevel: 'Beginner',
        }}
      />
      <Script
        data={{
          '@context': 'https://schema.org',
          '@type': 'BreadcrumbList',
          itemListElement: [
            {
              '@type': 'ListItem',
              position: 1,
              name: 'orm',
              item: `${SITE_URL}/${locale}/`,
            },
            ...(section
              ? [{ '@type': 'ListItem', position: 2, name: section }]
              : []),
            {
              '@type': 'ListItem',
              position: section ? 3 : 2,
              name: title,
              item: url,
            },
          ],
        }}
      />
    </>
  );
}
