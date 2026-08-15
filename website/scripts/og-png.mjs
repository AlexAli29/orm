/*
  The share cards, given a file extension.

  next/og generates them, and Next writes each one to a path with no extension:
  out/en/opengraph-image. Two things then go wrong on a static host, and both
  end the same way — no card.

  A path with no extension has nothing to infer a type from, so it is served as
  application/octet-stream and no crawler renders it. And trailingSlash makes
  /en/opengraph-image a 308 to /en/opengraph-image/, which crawlers do not follow
  for an image the way a browser follows one for a page.

  A file called og.png has neither problem — but it has to be somewhere the
  deploy will actually publish. Vercel's Next builder collects what next build
  produced and never looks at files added to out/ afterwards, which is why the
  first attempt at this shipped a 404: the copies existed locally and nowhere
  else.

  So they are written to public/ instead, which is a build input rather than a
  build output, and committed like the search index and the plain-text docs. The
  build after this one picks them up as ordinary static files with a real
  extension and no redirect.

  Generating them here rather than borrowing them would need satori and a
  rasterizer of our own; Next already has both.
*/
import fs from "node:fs/promises";
import path from "node:path";

const ROOT = path.join(path.dirname(new URL(import.meta.url).pathname), "..");
const OUT = path.join(ROOT, "out");
const PUBLIC = path.join(ROOT, "public");

const cards = [
  ["opengraph-image", "og.png"],
  ["en/opengraph-image", "en/og.png"],
  ["ru/opengraph-image", "ru/og.png"],
];

let copied = 0;
for (const [from, to] of cards) {
  const src = path.join(OUT, from);
  try {
    const bytes = await fs.readFile(src);
    // A PNG starts with the eight-byte signature. Copying something that is not
    // one would produce a card that 200s and renders as nothing, which is the
    // failure this script exists to end rather than repeat.
    if (bytes.subarray(1, 4).toString() !== "PNG") {
      throw new Error(`${from} is not a PNG`);
    }
    const dest = path.join(PUBLIC, to);
    await fs.mkdir(path.dirname(dest), { recursive: true });
    await fs.writeFile(dest, bytes);
    copied++;
  } catch (err) {
    console.error(`og: ${from} -> ${to}: ${err.message}`);
    process.exit(1);
  }
}
console.log(`og: refreshed ${copied} share cards in public/`);
