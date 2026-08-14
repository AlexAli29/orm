/*
  The root sends a reader to a language.

  Vercel does this as a redirect (see vercel.json) so the reader never sees this
  page. It exists for every other host, and for the case where JavaScript is off:
  the links are real links, not a spinner.
*/
import Link from "next/link";

export const metadata = { robots: { index: false } };

export default function Root() {
  return (
    <html lang="en">
      <head>
        <meta httpEquiv="refresh" content="0; url=/en/" />
      </head>
      <body style={{ fontFamily: "system-ui, sans-serif", padding: "3rem", textAlign: "center" }}>
        <p>
          <Link href="/en/">Documentation in English</Link>
        </p>
        <p>
          <Link href="/ru/">Документация на русском</Link>
        </p>
      </body>
    </html>
  );
}
