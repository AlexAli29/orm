import type { Metadata, Viewport } from 'next';
import { SITE_URL } from '@/lib/site';
import './globals.css';

const TITLE = 'orm — a schema-reconciling PostgreSQL mapper for Go';
const DESCRIPTION =
  'You own your structs. PostgreSQL owns your schema. The generator proves they agree.';

export const metadata: Metadata = {
  metadataBase: new URL(SITE_URL),
  title: { default: TITLE, template: '%s — orm' },
  description: DESCRIPTION,
  applicationName: 'orm',
  // Terms someone would actually type. A keyword list is worth little to a
  // search engine and something to the other machines that read this page.
  keywords: [
    'Go ORM',
    'Golang ORM',
    'PostgreSQL',
    'pgx',
    'type-safe SQL',
    'database migrations',
    'schema reconciliation',
    'query builder',
    'code generation',
    'PostGIS',
  ],
  authors: [{ name: 'AlexAli29', url: 'https://github.com/AlexAli29' }],
  creator: 'AlexAli29',
  openGraph: {
    type: 'website',
    siteName: 'orm',
    title: TITLE,
    description: DESCRIPTION,
    url: SITE_URL,
    // Named explicitly, and named with an extension. The generated
    // opengraph-image route has none, which a static host serves as
    // application/octet-stream and trailingSlash turns into a redirect —
    // two separate ways for a crawler to end up with no card.
    images: [{ url: '/og.png', width: 1200, height: 630, alt: TITLE }],
  },
  twitter: {
    card: 'summary_large_image',
    title: TITLE,
    description: DESCRIPTION,
    images: ['/og.png'],
  },
  robots: {
    index: true,
    follow: true,
    googleBot: {
      index: true,
      follow: true,
      // The default snippet limits are conservative for documentation, where a
      // longer preview is usually the answer the reader wanted.
      'max-snippet': -1,
      'max-image-preview': 'large',
      'max-video-preview': -1,
    },
  },
  icons: { icon: '/favicon.svg' },
  // Search Console ownership.
  //
  // The domain is vercel.app, which belongs to Vercel, so the DNS record a
  // Domain property asks for cannot be added — it would have to go in somebody
  // else's zone. A URL-prefix property verified by meta tag is the method that
  // fits, and the token comes from the environment so it is set in the deploy
  // rather than committed here.
  verification: {
    google: process.env.NEXT_PUBLIC_GOOGLE_SITE_VERIFICATION || undefined,
    other: process.env.NEXT_PUBLIC_BING_SITE_VERIFICATION
      ? { 'msvalidate.01': process.env.NEXT_PUBLIC_BING_SITE_VERIFICATION }
      : undefined,
  },
};

export const viewport: Viewport = {
  themeColor: [
    { media: '(prefers-color-scheme: light)', color: '#f4fbfd' },
    { media: '(prefers-color-scheme: dark)', color: '#04141a' },
  ],
  width: 'device-width',
  initialScale: 1,
};

/*
  The theme is applied before the first paint.

  A class written by an effect arrives after the browser has already painted the
  light theme, which is a white flash on every load for every dark-theme reader.
  This runs synchronously in the head instead, so the first paint is correct.
*/
const themeScript = `
try {
  var t = localStorage.getItem('theme');
  if (t === 'dark' || t === 'light') document.documentElement.setAttribute('data-theme', t);
} catch (e) {}
try {
  // The language of the document, corrected before anything reads it.
  //
  // Only one <html> exists and it lives in the root layout, which is above the
  // [locale] segment and cannot know which language it is wrapping. The static
  // markup therefore ships lang="en" for every page, and a screen reader would
  // pronounce the Russian pages as English — which is not a nuance, it is
  // unusable. This corrects the live DOM, and the hreflang links and og:locale
  // tell crawlers the same thing in the markup itself.
  if (location.pathname.indexOf('/ru') === 0) document.documentElement.lang = 'ru';
} catch (e) {}
`;

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html lang="en" suppressHydrationWarning>
      <head>
        <script dangerouslySetInnerHTML={{ __html: themeScript }} />
      </head>
      <body>
        <div className="aurora" aria-hidden>
          <span />
        </div>
        {children}
      </body>
    </html>
  );
}
