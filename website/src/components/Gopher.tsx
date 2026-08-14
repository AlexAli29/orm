/*
  The mascots.

  Drawn here rather than fetched, and original artwork in the spirit of the Go
  gopher rather than a copy of it. The Go gopher is Renée French's, licensed
  CC-BY 3.0; the credit is in the footer, and nothing here reproduces her
  drawing.

  Each is one inline SVG with no external file, so a gopher never arrives after
  the text it belongs beside.

  The drawing order is the whole trick. Ears go behind the head, arms behind
  whatever they hold, and the held object in front of the lower body — a shape
  layered in the wrong order stops reading as an object being held and starts
  reading as a box stuck to a chest.
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

/** The shared body gradient, once per drawing so ids never collide. */
function Skin({ id }: { id: string }) {
  return (
    <defs>
      <linearGradient id={id} x1="0" y1="0" x2="0" y2="1">
        <stop offset="0%" stopColor="#8fdff0" />
        <stop offset="55%" stopColor="#35bfdf" />
        <stop offset="100%" stopColor="#00a2cc" />
      </linearGradient>
    </defs>
  );
}

/**
 * A pair of eyes with the highlight in the same place on both.
 *
 * Kept as one component because the highlight offset is what makes a gopher
 * look alive, and two hand-placed copies drift apart the first time anything
 * moves.
 */
function Eyes({ cx, cy, r, gap }: { cx: number; cy: number; r: number; gap: number }) {
  const pupil = r * 0.42;
  return (
    <g>
      <ellipse cx={cx - gap} cy={cy} rx={r} ry={r * 1.06} fill="#fff" />
      <ellipse cx={cx + gap} cy={cy} rx={r} ry={r * 1.06} fill="#fff" />
      <circle cx={cx - gap + r * 0.26} cy={cy + r * 0.12} r={pupil} fill="#0b2027" />
      <circle cx={cx + gap + r * 0.26} cy={cy + r * 0.12} r={pupil} fill="#0b2027" />
      <circle cx={cx - gap + r * 0.42} cy={cy - r * 0.14} r={pupil * 0.34} fill="#fff" />
      <circle cx={cx + gap + r * 0.42} cy={cy - r * 0.14} r={pupil * 0.34} fill="#fff" />
    </g>
  );
}

/** Nose and the two incisors, sized off one number so they stay in proportion. */
function Snout({ cx, cy, s }: { cx: number; cy: number; s: number }) {
  return (
    <g>
      <ellipse cx={cx} cy={cy} rx={s * 0.42} ry={s * 0.3} fill="#0b2027" />
      <path
        d={`M${cx} ${cy + s * 0.3} v${s * 0.26}`}
        stroke="#0b2027"
        strokeWidth={s * 0.11}
        strokeLinecap="round"
      />
      <rect
        x={cx - s * 0.34}
        y={cy + s * 0.56}
        width={s * 0.32}
        height={s * 0.62}
        rx={s * 0.09}
        fill="#fff"
        stroke="#0b2027"
        strokeWidth={s * 0.07}
      />
      <rect
        x={cx + s * 0.02}
        y={cy + s * 0.56}
        width={s * 0.32}
        height={s * 0.62}
        rx={s * 0.09}
        fill="#fff"
        stroke="#0b2027"
        strokeWidth={s * 0.07}
      />
    </g>
  );
}

/** The face alone. */
export function GopherFace({ className, label }: GopherProps) {
  return (
    <svg viewBox="0 0 120 120" className={className} {...svgProps(label)}>
      <Skin id="gf" />
      <ellipse cx="24" cy="32" rx="12" ry="14" fill="url(#gf)" />
      <ellipse cx="96" cy="32" rx="12" ry="14" fill="url(#gf)" />
      <ellipse cx="24" cy="32" rx="5.5" ry="7" fill="#00758d" opacity="0.4" />
      <ellipse cx="96" cy="32" rx="5.5" ry="7" fill="#00758d" opacity="0.4" />
      <ellipse cx="60" cy="64" rx="45" ry="42" fill="url(#gf)" />
      <Eyes cx={60} cy={54} r={15} gap={17} />
      <Snout cx={60} cy={78} s={16} />
    </svg>
  );
}

/**
 * A gopher standing behind a database drum.
 *
 * The drum is in front of the lower body and the arms rest on its shoulders, so
 * it reads as something being held. Drawn the other way round — drum over the
 * chest, arms floating beside it — it reads as a box glued on, which is what the
 * first version of this did.
 */
export function GopherWithDatabase({ className, label }: GopherProps) {
  return (
    <svg viewBox="0 0 200 190" className={className} {...svgProps(label)}>
      <Skin id="gd" />
      <defs>
        <linearGradient id="gd-drum" x1="0" y1="0" x2="1" y2="1">
          <stop offset="0%" stopColor="#6fd4ea" />
          <stop offset="100%" stopColor="#0090b4" />
        </linearGradient>
      </defs>

      {/* ears, behind the head */}
      <ellipse cx="62" cy="34" rx="13" ry="15" fill="url(#gd)" />
      <ellipse cx="138" cy="34" rx="13" ry="15" fill="url(#gd)" />
      <ellipse cx="62" cy="34" rx="6" ry="7.5" fill="#00758d" opacity="0.4" />
      <ellipse cx="138" cy="34" rx="6" ry="7.5" fill="#00758d" opacity="0.4" />

      {/* feet, behind the drum */}
      <ellipse cx="76" cy="170" rx="15" ry="7.5" fill="#00758d" />
      <ellipse cx="124" cy="170" rx="15" ry="7.5" fill="#00758d" />

      {/* body */}
      <ellipse cx="100" cy="98" rx="52" ry="62" fill="url(#gd)" />

      <Eyes cx={100} cy={64} r={17} gap={19} />
      <Snout cx={100} cy={92} s={18} />

      {/* arms, behind the drum so they end at its edge */}
      <ellipse cx="56" cy="132" rx="11" ry="17" fill="url(#gd)" transform="rotate(-24 56 132)" />
      <ellipse cx="144" cy="132" rx="11" ry="17" fill="url(#gd)" transform="rotate(24 144 132)" />

      {/* the drum, in front of the lower body */}
      <g>
        <ellipse cx="100" cy="158" rx="34" ry="10" fill="#00758d" />
        <rect x="66" y="124" width="68" height="34" fill="url(#gd-drum)" />
        <ellipse cx="100" cy="124" rx="34" ry="10.5" fill="#8fe0f0" />
        <ellipse cx="100" cy="124" rx="24" ry="6.5" fill="#bff0fa" opacity="0.7" />
        <path d="M66 137 a34 10.5 0 0 0 68 0" fill="none" stroke="#e6f6fb" strokeWidth="1.8" opacity="0.7" />
        <path d="M66 148 a34 10.5 0 0 0 68 0" fill="none" stroke="#e6f6fb" strokeWidth="1.8" opacity="0.5" />
      </g>

      {/* paws over the drum's rim, which is what sells the hold */}
      <ellipse cx="70" cy="129" rx="9" ry="6.5" fill="#5dc9e2" transform="rotate(-18 70 129)" />
      <ellipse cx="130" cy="129" rx="9" ry="6.5" fill="#5dc9e2" transform="rotate(18 130 129)" />
    </svg>
  );
}

/**
 * A gopher with a magnifying glass, for the search palette's empty state.
 *
 * The glass is held out to one side rather than over the face: over the face it
 * covers an eye, and a one-eyed mascot is what a reader notices instead of the
 * message underneath.
 */
export function GopherSearching({ className, label }: GopherProps) {
  return (
    <svg viewBox="0 0 200 170" className={className} {...svgProps(label)}>
      <Skin id="gs" />
      <ellipse cx="52" cy="34" rx="11.5" ry="13.5" fill="url(#gs)" />
      <ellipse cx="118" cy="34" rx="11.5" ry="13.5" fill="url(#gs)" />
      <ellipse cx="52" cy="34" rx="5" ry="6.5" fill="#00758d" opacity="0.4" />
      <ellipse cx="118" cy="34" rx="5" ry="6.5" fill="#00758d" opacity="0.4" />

      <ellipse cx="85" cy="150" rx="14" ry="7" fill="#00758d" />
      <ellipse cx="85" cy="92" rx="47" ry="55" fill="url(#gs)" />

      <Eyes cx={85} cy={60} r={15} gap={17} />
      <Snout cx={85} cy={86} s={16} />

      {/* the arm reaching out, behind the glass */}
      <ellipse cx="132" cy="104" rx="10" ry="16" fill="url(#gs)" transform="rotate(52 132 104)" />

      <g transform="rotate(-18 156 96)">
        <rect x="151" y="118" width="9" height="34" rx="4.5" fill="#00758d" />
        <circle cx="156" cy="96" r="27" fill="#e6f6fb" opacity="0.5" />
        <circle cx="156" cy="96" r="27" fill="none" stroke="#00758d" strokeWidth="6" />
        <circle cx="156" cy="96" r="22" fill="none" stroke="#8fe0f0" strokeWidth="1.5" />
        <path d="M142 84 a19 19 0 0 1 13 -7" stroke="#fff" strokeWidth="3.5" fill="none" strokeLinecap="round" opacity="0.85" />
      </g>
    </svg>
  );
}

/** The small mark in the header lockup. */
export function GopherMark({ className, label }: GopherProps) {
  return (
    <svg viewBox="0 0 40 40" className={className} {...svgProps(label)}>
      <Skin id="gm" />
      <ellipse cx="9" cy="11" rx="4.6" ry="5.4" fill="url(#gm)" />
      <ellipse cx="31" cy="11" rx="4.6" ry="5.4" fill="url(#gm)" />
      <ellipse cx="20" cy="22.5" rx="15.5" ry="14.5" fill="url(#gm)" />
      <Eyes cx={20} cy={18.5} r={5.4} gap={6} />
      <Snout cx={20} cy={26} s={5.6} />
    </svg>
  );
}
