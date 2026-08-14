# orm documentation site

A static Next.js site: two languages, glass over a Go-blue aurora, and search
that runs in the browser.

## Running it

```bash
npm install
npm run dev     # http://localhost:3000
npm run build   # static export into out/
```

`npm run build` runs `scripts/build-search.mjs` first, which walks the content
and writes `public/search-en.json` and `public/search-ru.json`. Run it alone with
`npm run search` after editing content if you want the dev server's search to see
the change.

## Where things are

```
src/content/{en,ru}/**.md   the documentation — one file per page, shared slugs
src/lib/nav.ts              the sidebar, the locales and every UI string
src/lib/content.ts          markdown to HTML, with Shiki for code
src/components/Gopher.tsx   the mascots, drawn inline
src/components/Search.tsx   the palette and its scoring
scripts/build-search.mjs    the index builder
```

## Adding a page

1. Write `src/content/en/<slug>.md` **and** `src/content/ru/<slug>.md`. The slug
   is shared, which is what lets the language switch keep you on the page.
2. Add it to a section in `src/lib/nav.ts`.

Front matter is a title and a description, and nothing else:

```markdown
---
title: Composition
description: Joins, CTEs, derived tables and subqueries.
---
```

## Deploying to Vercel

The site lives in a subdirectory, so Vercel needs to be told where:

- **Root Directory:** `website`
- **Framework preset:** Next.js (detected)
- Build command and output directory: leave as detected

Then:

```bash
npm i -g vercel
cd website
vercel        # preview
vercel --prod # production
```

`vercel.json` redirects `/` to `/en`. `next.config.ts` sets `output: "export"`,
so the result is static files — no server, no functions, and nothing to keep
warm.

## The gophers

Original artwork drawn for this site, in the spirit of the Go gopher. The
original character is Renée French's, licensed CC BY 3.0. Nothing here
reproduces her drawing.
