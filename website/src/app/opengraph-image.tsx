/*
  The share card.

  Generated at build time rather than drawn by hand, so it cannot fall out of
  step with the wording it quotes. It is a PNG because that is what the crawlers
  accept: Twitter and Slack do not render SVG, and a card nobody can see is
  worse than no card, since the absence at least degrades to a plain link.

  No image is fetched and no font is loaded from anywhere. The runtime that
  renders this has no network, and a card that needs one is a build that fails
  on somebody else's outage.
*/
import { ImageResponse } from "next/og";

export const dynamic = "force-static";

export const size = { width: 1200, height: 630 };
export const contentType = "image/png";
export const alt = "orm — a schema-reconciling PostgreSQL mapper for Go";

export default function Image() {
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
        <div style={{ fontSize: 40, lineHeight: 1.3, marginTop: 28, color: "#9fd8e8" }}>
          You own your structs. PostgreSQL owns your schema.
        </div>
        <div style={{ fontSize: 40, lineHeight: 1.3, color: "#00ADD8", fontWeight: 600 }}>
          The generator proves they agree.
        </div>
        <div style={{ fontSize: 26, marginTop: 44, color: "#6b95a3" }}>
          PostgreSQL-native data mapper for Go · ormgo.vercel.app
        </div>
      </div>
    ),
    size,
  );
}
