/*
  The agent-readable docs, built before the site is.

  A browser reader gets HTML, a nav tree and a search palette. An agent gets one
  URL and whatever is behind it, so this writes the same documentation in the
  shape an agent can actually consume:

    /llms.txt            the index — every page, one line each, per llmstxt.org
    /llms-full.txt       every English page inline, one fetch
    /llms-full.ru.txt    the same in Russian
    /en/docs/<slug>.md   one page as its source markdown
    /api/orm.txt         the generated public API manifest

  The manifest is the part that matters, and it is why this exists as more than
  a convenience. Everything an agent writes against this library is a guess about
  which symbols exist, and a plausible guess — orm.Returning, EqCol, Users.Table()
  — compiles into a support request. The manifest is generated from the packages
  themselves and is exhaustive, so it answers that question exactly rather than
  approximately. llms.txt points at it in the first paragraph, and the per-page
  markdown links back to it, because an agent that reads one page should still
  learn where the ground truth is.

  Nothing here is hand-maintained. The page list comes from nav.ts, the prose
  from the same markdown the site renders, and the manifest is copied from the
  repository root where CI regenerates and diffs it. A page that exists on the
  site and not here would be a page the generator did not know about, which the
  build fails on rather than shipping quietly.
*/
import fs from "node:fs/promises";
import path from "node:path";

const ROOT = path.dirname(new URL(import.meta.url).pathname);
const CONTENT = path.join(ROOT, "..", "src", "content");
const PUBLIC = path.join(ROOT, "..", "public");
const REPO = path.join(ROOT, "..", "..");

// Read from src/lib/site.ts rather than repeated here, so the URLs baked into
// llms.txt and the ones in the page metadata cannot drift apart.
const SITE = (await fs.readFile(path.join(ROOT, "..", "src", "lib", "site.ts"), "utf8")).match(
  /SITE_URL\s*=\s*"([^"]+)"/,
)?.[1];
if (!SITE) throw new Error("could not read SITE_URL out of src/lib/site.ts");
const LOCALES = ["en", "ru"];

// The manifests CI regenerates, and what each one is for. An agent asking
// "does this symbol exist" needs to be told which file to ask.
const MANIFESTS = [
  ["orm.txt", "The ORM itself. Every exported symbol in github.com/AlexAli29/orm."],
  ["ormtest-postgres.txt", "The test helpers, github.com/AlexAli29/orm/ormtest/postgres."],
  ["ormotel.txt", "The OpenTelemetry integration, github.com/AlexAli29/orm/ormotel."],
];

const HEADER = {
  en: {
    tagline:
      "A PostgreSQL-native ORM for Go. You own your structs, PostgreSQL owns your schema, and the generator proves they agree.",
    manifestNote:
      "Before writing code against this library, read /api/orm.txt. It is generated from the packages and lists every exported symbol. If a name is not in it, it does not exist — no matter how plausible it looks.",
  },
  ru: {
    tagline:
      "PostgreSQL-нативная ORM для Go. Структуры ваши, схема — PostgreSQL, а генератор доказывает, что они согласованы.",
    manifestNote:
      "Перед тем как писать код, прочитайте /api/orm.txt. Он порождён из самих пакетов и перечисляет каждый экспортированный символ. Если имени там нет — его не существует, каким бы правдоподобным оно ни выглядело.",
  },
};

function stripFrontMatter(raw) {
  if (!raw.startsWith("---")) return { meta: {}, body: raw };
  const end = raw.indexOf("\n---", 3);
  if (end < 0) return { meta: {}, body: raw };
  const meta = {};
  for (const line of raw.slice(3, end).split("\n")) {
    const i = line.indexOf(":");
    if (i < 0) continue;
    let v = line.slice(i + 1).trim();
    if ((v.startsWith('"') && v.endsWith('"')) || (v.startsWith("'") && v.endsWith("'"))) {
      v = v.slice(1, -1);
    }
    meta[line.slice(0, i).trim()] = v;
  }
  return { meta, body: raw.slice(end + 4).trim() };
}

// nav.ts is TypeScript, and this script is not compiled. The nav is pure data
// though — strings, arrays and object literals, nothing computed — so the array
// literal is evaluated rather than pattern-matched. A regex over source is the
// kind of parser that works until someone reformats the file.
async function readNav() {
  const src = await fs.readFile(path.join(ROOT, "..", "src", "lib", "nav.ts"), "utf8");
  const decl = src.indexOf("export const nav");
  if (decl < 0) throw new Error("nav.ts has no `export const nav`");

  const start = src.indexOf("[", decl);
  const end = src.indexOf("\n];", start);
  if (start < 0 || end < 0) throw new Error("could not find the bounds of the nav array");

  const sections = new Function(`return ${src.slice(start, end + 2)}`)();
  if (!Array.isArray(sections) || !sections.length) throw new Error("nav.ts evaluated to no sections");
  return sections;
}

async function main() {
  const sections = await readNav();
  const slugs = sections.flatMap((s) => s.items.map((p) => p.slug));

  // The parse above is only trustworthy if it found everything. A page on disk
  // and not in the nav would be missing from llms.txt, which is the failure
  // this whole file exists to avoid.
  for (const locale of LOCALES) {
    // Recursive: the cookbook pages live in a subdirectory, and their slugs
    // carry it — "cookbook/queries", not "queries".
    const onDisk = (await fs.readdir(path.join(CONTENT, locale), { recursive: true }))
      .filter((f) => f.endsWith(".md"))
      .map((f) => f.slice(0, -3))
      .sort();
    const missing = onDisk.filter((s) => !slugs.includes(s));
    const absent = slugs.filter((s) => !onDisk.includes(s));
    if (missing.length) throw new Error(`${locale}: pages not reached from nav.ts: ${missing.join(", ")}`);
    if (absent.length) throw new Error(`${locale}: nav.ts lists pages with no markdown: ${absent.join(", ")}`);
  }

  const pages = {};
  for (const locale of LOCALES) {
    pages[locale] = {};
    for (const slug of slugs) {
      const raw = await fs.readFile(path.join(CONTENT, locale, `${slug}.md`), "utf8");
      pages[locale][slug] = stripFrontMatter(raw);
    }
  }

  await writeManifests();
  await writePerPage(pages, slugs);
  for (const locale of LOCALES) {
    await writeIndex(locale, sections, pages[locale]);
    await writeFull(locale, sections, pages[locale]);
  }
}

async function writeManifests() {
  await fs.mkdir(path.join(PUBLIC, "api"), { recursive: true });
  // The manifests live at the repository root, where CI regenerates and diffs
  // them. The copies under public/ are committed, so a build that cannot see
  // the root — a deploy configured with website/ as its only source, say —
  // still ships the last known-good manifest rather than failing outright. It
  // says so loudly, because a stale symbol list is worse than a missing one is
  // obvious.
  let copied = 0;
  for (const [name] of MANIFESTS) {
    try {
      await fs.copyFile(path.join(REPO, "api", name), path.join(PUBLIC, "api", name));
      copied++;
    } catch (err) {
      if (err.code !== "ENOENT") throw err;
      await fs.access(path.join(PUBLIC, "api", name));
      console.warn(`llms: WARNING ${name} not found at the repo root; serving the committed copy`);
    }
  }
  console.log(`llms: copied ${copied}/${MANIFESTS.length} API manifests`);
}

// One file per page, so an agent that has been pointed at a single topic can
// fetch the source of it rather than scraping the rendered page. The URL is the
// page's own URL with .md on the end.
async function writePerPage(pages, slugs) {
  let n = 0;
  for (const locale of LOCALES) {
    const dir = path.join(PUBLIC, locale, "docs");
    await fs.mkdir(dir, { recursive: true });
    for (const slug of slugs) {
      const { meta, body } = pages[locale][slug];
      await fs.mkdir(path.join(dir, path.dirname(slug)), { recursive: true });
      const head = [
        `# ${meta.title ?? slug}`,
        meta.description ? `\n> ${meta.description}` : "",
        `\nSource: ${SITE}/${locale}/docs/${slug}/`,
        `Symbols: ${SITE}/api/orm.txt — the generated list of every exported name.`,
        "",
        "---",
        "",
      ].join("\n");
      await fs.writeFile(path.join(dir, `${slug}.md`), head + body + "\n");
      n++;
    }
  }
  console.log(`llms: wrote ${n} per-page markdown files`);
}

async function writeIndex(locale, sections, pages) {
  const { tagline, manifestNote } = HEADER[locale];
  const out = [`# ORM`, ``, `> ${tagline}`, ``, manifestNote, ``];

  out.push(
    locale === "en"
      ? `The whole of these docs in one file: ${SITE}/llms-full.txt`
      : `Вся документация одним файлом: ${SITE}/llms-full.ru.txt`,
    ``,
  );

  for (const section of sections) {
    out.push(`## ${section.title[locale]}`, ``);
    for (const page of section.items) {
      const desc = pages[page.slug].meta.description ?? "";
      out.push(`- [${page.title[locale]}](${SITE}/${locale}/docs/${page.slug}.md)${desc ? `: ${desc}` : ""}`);
    }
    out.push(``);
  }

  out.push(locale === "en" ? `## Generated API manifests` : `## Порождённые манифесты API`, ``);
  for (const [name, what] of MANIFESTS) {
    out.push(`- [${name}](${SITE}/api/${name}): ${what}`);
  }
  out.push(``);

  const file = locale === "en" ? "llms.txt" : "llms.ru.txt";
  await fs.writeFile(path.join(PUBLIC, file), out.join("\n"));
  console.log(`llms: ${file} — ${sections.reduce((n, s) => n + s.items.length, 0)} pages`);
}

async function writeFull(locale, sections, pages) {
  const { tagline, manifestNote } = HEADER[locale];
  const out = [
    `# ORM — ${locale === "en" ? "complete documentation" : "полная документация"}`,
    ``,
    `> ${tagline}`,
    ``,
    manifestNote,
    ``,
    `${SITE}/api/orm.txt`,
    ``,
  ];

  for (const section of sections) {
    for (const page of section.items) {
      const { meta, body } = pages[page.slug];
      out.push(
        `\n\n---\n`,
        `# ${meta.title ?? page.slug}`,
        ``,
        meta.description ? `> ${meta.description}\n` : ``,
        `${SITE}/${locale}/docs/${page.slug}/`,
        ``,
        body,
      );
    }
  }

  const file = locale === "en" ? "llms-full.txt" : "llms-full.ru.txt";
  const text = out.join("\n") + "\n";
  await fs.writeFile(path.join(PUBLIC, file), text);
  console.log(`llms: ${file} — ${Math.round(text.length / 1024)} kB`);
}

await main();
