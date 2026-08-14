/*
  The search index, built before the site is.

  One record per page and one per H2/H3 section, so a hit can land on the heading
  it is about rather than on the top of a long page. The body text is stripped of
  code and markup: a reader searching for "Opt" wants the sentence explaining it,
  not forty lines of Go that happen to contain the token.

  It is written to public/ rather than imported, so the index ships as a file the
  browser fetches once — on the first search, not on the first page load.
*/
import fs from "node:fs/promises";
import path from "node:path";

const ROOT = path.dirname(new URL(import.meta.url).pathname);
const CONTENT = path.join(ROOT, "..", "src", "content");
const OUT = path.join(ROOT, "..", "public");

const LOCALES = ["en", "ru"];

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
  return { meta, body: raw.slice(end + 4) };
}

function slugify(s) {
  return s
    .toLowerCase()
    .trim()
    .replace(/[^\p{L}\p{N}\s-]/gu, "")
    .replace(/\s+/g, "-");
}

function clean(s) {
  return s
    .replace(/```[\s\S]*?```/g, " ")   // fenced code
    .replace(/`[^`]*`/g, " ")           // inline code
    .replace(/!?\[([^\]]*)\]\([^)]*\)/g, "$1") // links keep their text
    .replace(/^[|>#*+-]+/gm, " ")
    .replace(/[*_~]/g, "")
    .replace(/\s+/g, " ")
    .trim();
}

async function walk(dir, base = "") {
  const out = [];
  for (const entry of await fs.readdir(dir, { withFileTypes: true })) {
    const rel = base ? `${base}/${entry.name}` : entry.name;
    if (entry.isDirectory()) out.push(...(await walk(path.join(dir, entry.name), rel)));
    else if (entry.name.endsWith(".md")) out.push(rel.replace(/\.md$/, ""));
  }
  return out;
}

for (const locale of LOCALES) {
  const dir = path.join(CONTENT, locale);
  let slugs;
  try {
    slugs = await walk(dir);
  } catch {
    console.warn(`search: no content for ${locale}`);
    continue;
  }

  const records = [];
  for (const slug of slugs.sort()) {
    const raw = await fs.readFile(path.join(dir, `${slug}.md`), "utf8");
    const { meta, body } = stripFrontMatter(raw);
    const title = meta.title ?? slug;

    // Split on H2/H3, keeping the heading with the text beneath it.
    const parts = body.split(/^(#{2,3})\s+(.+)$/m);
    const intro = clean(parts[0] ?? "");
    records.push({ s: slug, t: title, h: "", i: "", b: `${meta.description ?? ""} ${intro}`.trim() });

    for (let i = 1; i < parts.length; i += 3) {
      const heading = (parts[i + 1] ?? "").trim();
      const text = clean(parts[i + 2] ?? "");
      if (!heading) continue;
      records.push({ s: slug, t: title, h: heading, i: slugify(heading), b: text });
    }
  }

  await fs.mkdir(OUT, { recursive: true });
  const file = path.join(OUT, `search-${locale}.json`);
  await fs.writeFile(file, JSON.stringify(records));
  const kb = (JSON.stringify(records).length / 1024).toFixed(0);
  console.log(`search: ${locale} — ${records.length} records, ${kb} kB`);
}
