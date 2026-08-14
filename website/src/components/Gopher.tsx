/*
  The mascots.

  These are drawn here rather than fetched, and they are original artwork in the
  spirit of the Go gopher rather than a copy of it. The Go gopher is Renée
  French's, licensed CC-BY 3.0; the credit is on the About page, and nothing here
  reproduces her drawing.

  Each one is a single inline SVG with no external file, so a gopher never
  arrives after the text it belongs beside. They take currentColor for the
  outline and the Go palette for everything else.
*/

type GopherProps = {
  className?: string;
  /** Decorative by default; pass a label when the drawing carries meaning. */
  label?: string;
};

function svgProps(label?: string) {
  return label
    ? ({ role: "img", "aria-label": label } as const)
    : ({ "aria-hidden": true, focusable: false } as const);
}

/** The face. Two teeth, wide eyes, the ORM's blue. */
export function GopherFace({ className, label }: GopherProps) {
  return (
    <svg viewBox="0 0 120 120" className={className} {...svgProps(label)}>
      <defs>
        <linearGradient id="gf-body" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stopColor="#7fd8ea" />
          <stop offset="100%" stopColor="#00add8" />
        </linearGradient>
      </defs>
      {/* ears */}
      <ellipse cx="26" cy="34" rx="13" ry="15" fill="url(#gf-body)" />
      <ellipse cx="94" cy="34" rx="13" ry="15" fill="url(#gf-body)" />
      <ellipse cx="26" cy="34" rx="6" ry="7.5" fill="#00758d" opacity="0.45" />
      <ellipse cx="94" cy="34" rx="6" ry="7.5" fill="#00758d" opacity="0.45" />
      {/* head */}
      <ellipse cx="60" cy="64" rx="44" ry="42" fill="url(#gf-body)" />
      {/* eyes */}
      <ellipse cx="43" cy="55" rx="15" ry="16" fill="#fff" />
      <ellipse cx="77" cy="55" rx="15" ry="16" fill="#fff" />
      <circle cx="47" cy="57" r="6.5" fill="#0b2027" />
      <circle cx="73" cy="57" r="6.5" fill="#0b2027" />
      <circle cx="49.2" cy="54.6" r="2.2" fill="#fff" />
      <circle cx="75.2" cy="54.6" r="2.2" fill="#fff" />
      {/* muzzle */}
      <ellipse cx="60" cy="79" rx="13" ry="10" fill="#fff" opacity="0.92" />
      <ellipse cx="60" cy="72" rx="4.6" ry="3.4" fill="#0b2027" />
      {/* teeth */}
      <rect x="55.4" y="77" width="4" height="8.5" rx="1.2" fill="#fff" stroke="#0b2027" strokeWidth="1" />
      <rect x="60.6" y="77" width="4" height="8.5" rx="1.2" fill="#fff" stroke="#0b2027" strokeWidth="1" />
      {/* whiskers */}
      <g stroke="#0b2027" strokeWidth="1.4" strokeLinecap="round" opacity="0.55">
        <path d="M46 76 L30 72" />
        <path d="M46 80 L30 82" />
        <path d="M74 76 L90 72" />
        <path d="M74 80 L90 82" />
      </g>
    </svg>
  );
}

/**
 * A gopher holding a database drum — the reconciliation mascot.
 *
 * It appears where the page is about the schema being proved rather than
 * guessed, so the drum is drawn as something the gopher is holding rather than
 * standing on.
 */
export function GopherWithDatabase({ className, label }: GopherProps) {
  return (
    <svg viewBox="0 0 200 180" className={className} {...svgProps(label)}>
      <defs>
        <linearGradient id="gd-body" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stopColor="#7fd8ea" />
          <stop offset="100%" stopColor="#00add8" />
        </linearGradient>
        <linearGradient id="gd-drum" x1="0" y1="0" x2="1" y2="1">
          <stop offset="0%" stopColor="#5dc9e2" />
          <stop offset="100%" stopColor="#00758d" />
        </linearGradient>
      </defs>
      {/* feet */}
      <ellipse cx="76" cy="163" rx="14" ry="7" fill="#00758d" />
      <ellipse cx="118" cy="163" rx="14" ry="7" fill="#00758d" />
      {/* ears */}
      <ellipse cx="66" cy="36" rx="12" ry="14" fill="url(#gd-body)" />
      <ellipse cx="128" cy="36" rx="12" ry="14" fill="url(#gd-body)" />
      {/* body */}
      <ellipse cx="97" cy="98" rx="52" ry="62" fill="url(#gd-body)" />
      {/* eyes */}
      <ellipse cx="80" cy="62" rx="16" ry="17" fill="#fff" />
      <ellipse cx="116" cy="62" rx="16" ry="17" fill="#fff" />
      <circle cx="84" cy="64" r="7" fill="#0b2027" />
      <circle cx="112" cy="64" r="7" fill="#0b2027" />
      <circle cx="86.4" cy="61.4" r="2.4" fill="#fff" />
      <circle cx="114.4" cy="61.4" r="2.4" fill="#fff" />
      {/* muzzle and teeth */}
      <ellipse cx="98" cy="87" rx="14" ry="10" fill="#fff" opacity="0.92" />
      <ellipse cx="98" cy="80" rx="4.8" ry="3.5" fill="#0b2027" />
      <rect x="93.2" y="85" width="4.2" height="9" rx="1.2" fill="#fff" stroke="#0b2027" strokeWidth="1" />
      <rect x="98.6" y="85" width="4.2" height="9" rx="1.2" fill="#fff" stroke="#0b2027" strokeWidth="1" />
      {/* arms around the drum */}
      <ellipse cx="55" cy="112" rx="10" ry="16" fill="url(#gd-body)" transform="rotate(-18 55 112)" />
      <ellipse cx="139" cy="112" rx="10" ry="16" fill="url(#gd-body)" transform="rotate(18 139 112)" />
      {/* the database drum */}
      <g transform="translate(0,4)">
        <rect x="70" y="108" width="56" height="34" rx="4" fill="url(#gd-drum)" />
        <ellipse cx="98" cy="108" rx="28" ry="9" fill="#8fe0f0" />
        <ellipse cx="98" cy="142" rx="28" ry="9" fill="#00758d" />
        <path d="M70 120 a28 9 0 0 0 56 0" fill="none" stroke="#e6f6fb" strokeWidth="1.6" opacity="0.75" />
        <path d="M70 131 a28 9 0 0 0 56 0" fill="none" stroke="#e6f6fb" strokeWidth="1.6" opacity="0.55" />
      </g>
    </svg>
  );
}

/**
 * A gopher peering through a magnifying glass — the search and introspection
 * mascot. Used on the empty state of the search palette, where a picture is
 * doing the work a sentence would do badly.
 */
export function GopherSearching({ className, label }: GopherProps) {
  return (
    <svg viewBox="0 0 180 160" className={className} {...svgProps(label)}>
      <defs>
        <linearGradient id="gs-body" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stopColor="#7fd8ea" />
          <stop offset="100%" stopColor="#00add8" />
        </linearGradient>
      </defs>
      <ellipse cx="58" cy="34" rx="11" ry="13" fill="url(#gs-body)" />
      <ellipse cx="112" cy="34" rx="11" ry="13" fill="url(#gs-body)" />
      <ellipse cx="85" cy="88" rx="48" ry="56" fill="url(#gs-body)" />
      <ellipse cx="69" cy="58" rx="15" ry="16" fill="#fff" />
      <ellipse cx="103" cy="58" rx="15" ry="16" fill="#fff" />
      <circle cx="73" cy="60" r="6.5" fill="#0b2027" />
      <circle cx="99" cy="60" r="6.5" fill="#0b2027" />
      <circle cx="75.2" cy="57.6" r="2.2" fill="#fff" />
      <circle cx="101.2" cy="57.6" r="2.2" fill="#fff" />
      <ellipse cx="86" cy="80" rx="13" ry="9" fill="#fff" opacity="0.92" />
      <ellipse cx="86" cy="74" rx="4.4" ry="3.2" fill="#0b2027" />
      <rect x="81.6" y="78" width="4" height="8" rx="1.2" fill="#fff" stroke="#0b2027" strokeWidth="1" />
      <rect x="86.8" y="78" width="4" height="8" rx="1.2" fill="#fff" stroke="#0b2027" strokeWidth="1" />
      {/* magnifying glass */}
      <g transform="rotate(-20 130 96)">
        <circle cx="130" cy="96" r="26" fill="#e6f6fb" opacity="0.45" stroke="#00758d" strokeWidth="5" />
        <circle cx="130" cy="96" r="26" fill="none" stroke="#5dc9e2" strokeWidth="1.5" />
        <rect x="126" y="122" width="8" height="30" rx="4" fill="#00758d" />
        <path d="M116 84 a18 18 0 0 1 12 -6" stroke="#fff" strokeWidth="3" fill="none" strokeLinecap="round" opacity="0.8" />
      </g>
    </svg>
  );
}

/** A small gopher silhouette for the header lockup. */
export function GopherMark({ className, label }: GopherProps) {
  return (
    <svg viewBox="0 0 40 40" className={className} {...svgProps(label)}>
      <defs>
        <linearGradient id="gm-body" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stopColor="#7fd8ea" />
          <stop offset="100%" stopColor="#00add8" />
        </linearGradient>
      </defs>
      <ellipse cx="9.5" cy="11" rx="4.6" ry="5.4" fill="url(#gm-body)" />
      <ellipse cx="30.5" cy="11" rx="4.6" ry="5.4" fill="url(#gm-body)" />
      <ellipse cx="20" cy="22" rx="15.5" ry="14.5" fill="url(#gm-body)" />
      <ellipse cx="14" cy="18.5" rx="5.4" ry="5.8" fill="#fff" />
      <ellipse cx="26" cy="18.5" rx="5.4" ry="5.8" fill="#fff" />
      <circle cx="15.4" cy="19.3" r="2.4" fill="#0b2027" />
      <circle cx="27.4" cy="19.3" r="2.4" fill="#0b2027" />
      <ellipse cx="20" cy="27" rx="4.6" ry="3.4" fill="#fff" opacity="0.92" />
      <ellipse cx="20" cy="24.6" rx="1.7" ry="1.25" fill="#0b2027" />
      <rect x="18.4" y="26.4" width="1.5" height="3.2" rx="0.5" fill="#fff" stroke="#0b2027" strokeWidth="0.4" />
      <rect x="20.3" y="26.4" width="1.5" height="3.2" rx="0.5" fill="#fff" stroke="#0b2027" strokeWidth="0.4" />
    </svg>
  );
}
