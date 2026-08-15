/*
  A share card per language.

  Next refuses a metadata route under a catch-all segment, so there is no way to
  give every docs page its own card from the file convention — that would need a
  rasterizer of our own and a build step to go with it. A card per language is
  the useful half of it for none of that cost: a Russian reader sharing a link
  gets a Russian card, and the pages below inherit the nearest one.
*/
import { ImageResponse } from "next/og";
import { locales, isLocale, type Locale } from "@/lib/nav";

export const dynamic = "force-static";
export const size = { width: 1200, height: 630 };
export const contentType = "image/png";

export function generateStaticParams() {
  return locales.map((locale) => ({ locale }));
}

const copy: Record<Locale, { lead: string; punch: string; foot: string }> = {
  en: {
    lead: "You own your structs. PostgreSQL owns your schema.",
    punch: "The generator proves they agree.",
    foot: "PostgreSQL-native data mapper for Go · documentation",
  },
  ru: {
    lead: "Структуры ваши. Схема — PostgreSQL.",
    punch: "Генератор доказывает, что они согласованы.",
    foot: "PostgreSQL-нативный маппер данных для Go · документация",
  },
};

export default async function Image({ params }: { params: Promise<{ locale: string }> }) {
  const { locale: raw } = await params;
  const locale: Locale = isLocale(raw) ? raw : "en";
  const { lead, punch, foot } = copy[locale];

  return new ImageResponse(
    (
      <div
        style={{
          width: "100%",
          height: "100%",
          display: "flex",
          flexDirection: "column",
          justifyContent: "center",
          padding: "0 96px",
          background: "linear-gradient(135deg, #04141a 0%, #06222c 55%, #00394a 100%)",
          color: "#eaf7fb",
          fontFamily: "sans-serif",
        }}
      >
        <div style={{ display: "flex", alignItems: "center", gap: 20 }}>
          <div style={{ width: 26, height: 26, borderRadius: 999, background: "#00ADD8" }} />
          <div style={{ fontSize: 96, fontWeight: 700, letterSpacing: -2 }}>orm</div>
        </div>
        <div style={{ fontSize: 40, lineHeight: 1.3, marginTop: 28, color: "#9fd8e8" }}>{lead}</div>
        <div style={{ fontSize: 40, lineHeight: 1.3, color: "#00ADD8", fontWeight: 600 }}>{punch}</div>
        <div style={{ fontSize: 26, marginTop: 44, color: "#6b95a3" }}>{`${foot} · ormgo.vercel.app`}</div>
      </div>
    ),
    size,
  );
}
