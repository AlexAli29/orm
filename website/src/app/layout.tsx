import type { Metadata, Viewport } from "next";
import { SITE_URL } from "@/lib/site";
import "./globals.css";

export const metadata: Metadata = {
  metadataBase: new URL(SITE_URL),
  title: { default: "orm — a schema-reconciling PostgreSQL mapper for Go", template: "%s — orm" },
  description:
    "You own your structs. PostgreSQL owns your schema. The generator proves they agree.",
  icons: { icon: "/favicon.svg" },
};

export const viewport: Viewport = {
  themeColor: [
    { media: "(prefers-color-scheme: light)", color: "#f4fbfd" },
    { media: "(prefers-color-scheme: dark)", color: "#04141a" },
  ],
  width: "device-width",
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
`;

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html suppressHydrationWarning>
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
