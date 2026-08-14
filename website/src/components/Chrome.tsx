"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { nav, t, type Locale, locales } from "@/lib/nav";
import { GopherMark } from "./Gopher";

/* ---------------------------------------------------------------- theme --- */

/**
 * The theme toggle cycles light, dark and system.
 *
 * System is a real third state rather than the absence of a choice: a reader who
 * has not chosen follows their OS, and a reader who chose system is saying so.
 * The choice is written to localStorage and applied by the inline script in the
 * document head, so the first paint is already right and nothing flashes.
 */
export function ThemeToggle({ locale }: { locale: Locale }) {
  const [mode, setMode] = useState<"light" | "dark" | "system">("system");

  useEffect(() => {
    const saved = localStorage.getItem("theme");
    setMode(saved === "light" || saved === "dark" ? saved : "system");
  }, []);

  function apply(next: "light" | "dark" | "system") {
    setMode(next);
    const root = document.documentElement;
    if (next === "system") {
      localStorage.removeItem("theme");
      root.removeAttribute("data-theme");
    } else {
      localStorage.setItem("theme", next);
      root.setAttribute("data-theme", next);
    }
  }

  const order = ["light", "dark", "system"] as const;

  return (
    <button
      type="button"
      onClick={() => apply(order[(order.indexOf(mode) + 1) % order.length])}
      className="grid h-9 w-9 place-items-center rounded-xl border border-[var(--rule)] bg-[var(--glass)] text-[var(--fg-muted)] transition hover:text-[var(--accent)]"
      aria-label={t("theme", locale)}
      title={t("theme", locale)}
    >
      {mode === "light" && (
        <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round">
          <circle cx="12" cy="12" r="4" />
          <path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4" />
        </svg>
      )}
      {mode === "dark" && (
        <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
          <path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8Z" />
        </svg>
      )}
      {mode === "system" && (
        <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
          <rect x="2" y="4" width="20" height="13" rx="2" />
          <path d="M8 21h8M12 17v4" />
        </svg>
      )}
    </button>
  );
}

/* ------------------------------------------------------------- language --- */

const localeNames: Record<Locale, string> = { en: "English", ru: "Русский" };
const localeShort: Record<Locale, string> = { en: "EN", ru: "RU" };

/**
 * Switching language keeps the page.
 *
 * The slug is shared between the two content trees, so the same path exists in
 * both — swapping the first segment is the whole operation, and a reader
 * comparing wordings does not lose their place.
 */
export function LanguageSwitch({ locale }: { locale: Locale }) {
  const pathname = usePathname() || `/${locale}/`;
  const rest = pathname.split("/").slice(2).join("/");

  return (
    <div className="flex items-center rounded-xl border border-[var(--rule)] bg-[var(--glass)] p-0.5">
      {locales.map((l) => (
        <Link
          key={l}
          href={`/${l}/${rest}`}
          hrefLang={l}
          aria-current={l === locale ? "true" : undefined}
          title={localeNames[l]}
          className={`rounded-lg px-2.5 py-1 text-xs font-semibold transition ${
            l === locale
              ? "bg-[var(--accent-soft)] text-[var(--accent)]"
              : "text-[var(--fg-faint)] hover:text-[var(--fg)]"
          }`}
        >
          {localeShort[l]}
        </Link>
      ))}
    </div>
  );
}

/* -------------------------------------------------------------- sidebar --- */

export function Sidebar({
  locale,
  onNavigate,
}: {
  locale: Locale;
  onNavigate?: () => void;
}) {
  const pathname = usePathname() || "";

  return (
    <nav aria-label={t("docs", locale)} className="space-y-7 pb-16">
      {nav.map((section) => (
        <div key={section.title.en}>
          <h2 className="mb-2 px-3 text-[0.7rem] font-bold uppercase tracking-widest text-[var(--fg-faint)]">
            {section.title[locale]}
          </h2>
          <ul className="space-y-0.5">
            {section.items.map((item) => {
              const href = `/${locale}/docs/${item.slug}/`;
              const active = pathname === href || pathname === href.slice(0, -1);
              return (
                <li key={item.slug}>
                  <Link
                    href={href}
                    onClick={onNavigate}
                    aria-current={active ? "page" : undefined}
                    className={`block rounded-lg px-3 py-1.5 text-sm transition ${
                      active
                        ? "bg-[var(--accent-soft)] font-semibold text-[var(--accent)]"
                        : "text-[var(--fg-muted)] hover:bg-[var(--accent-soft)] hover:text-[var(--fg)]"
                    }`}
                  >
                    {item.title[locale]}
                  </Link>
                </li>
              );
            })}
          </ul>
        </div>
      ))}
    </nav>
  );
}

/* ------------------------------------------------------------------ toc --- */

/**
 * The table of contents highlights the heading you are reading.
 *
 * IntersectionObserver rather than a scroll handler: the browser does the work,
 * and the rootMargin puts the trigger line a third of the way down so a heading
 * lights up as it reaches reading position rather than as it leaves the top.
 */
export function TableOfContents({
  headings,
  locale,
}: {
  headings: { id: string; text: string; depth: 2 | 3 }[];
  locale: Locale;
}) {
  const [active, setActive] = useState<string>("");

  useEffect(() => {
    if (headings.length === 0) return;
    const observer = new IntersectionObserver(
      (entries) => {
        const visible = entries
          .filter((e) => e.isIntersecting)
          .sort((a, b) => a.boundingClientRect.top - b.boundingClientRect.top);
        if (visible[0]) setActive(visible[0].target.id);
      },
      { rootMargin: "-90px 0px -66% 0px", threshold: 0 },
    );
    for (const h of headings) {
      const el = document.getElementById(h.id);
      if (el) observer.observe(el);
    }
    return () => observer.disconnect();
  }, [headings]);

  if (headings.length === 0) return null;

  return (
    <nav aria-label={t("onThisPage", locale)} className="text-sm">
      <h2 className="mb-3 text-[0.7rem] font-bold uppercase tracking-widest text-[var(--fg-faint)]">
        {t("onThisPage", locale)}
      </h2>
      <ul className="space-y-1 border-l border-[var(--rule)]">
        {headings.map((h) => (
          <li key={h.id}>
            <a
              href={`#${h.id}`}
              className={`-ml-px block border-l-2 py-1 transition ${
                h.depth === 3 ? "pl-6" : "pl-3"
              } ${
                active === h.id
                  ? "border-[var(--accent)] font-medium text-[var(--accent)]"
                  : "border-transparent text-[var(--fg-faint)] hover:text-[var(--fg)]"
              }`}
            >
              {h.text}
            </a>
          </li>
        ))}
      </ul>
    </nav>
  );
}

/* -------------------------------------------------------- mobile drawer --- */

export function MobileNav({ locale }: { locale: Locale }) {
  const [open, setOpen] = useState(false);
  const pathname = usePathname();

  useEffect(() => setOpen(false), [pathname]);
  useEffect(() => {
    document.body.style.overflow = open ? "hidden" : "";
    return () => {
      document.body.style.overflow = "";
    };
  }, [open]);

  return (
    <>
      <button
        type="button"
        onClick={() => setOpen(true)}
        className="grid h-9 w-9 place-items-center rounded-xl border border-[var(--rule)] bg-[var(--glass)] text-[var(--fg-muted)] lg:hidden"
        aria-label={t("menu", locale)}
        aria-expanded={open}
      >
        <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round">
          <path d="M3 6h18M3 12h18M3 18h18" />
        </svg>
      </button>

      {open && (
        <div className="fixed inset-0 z-50 lg:hidden">
          <div
            className="absolute inset-0 bg-black/40 backdrop-blur-sm"
            onClick={() => setOpen(false)}
            aria-hidden
          />
          <div className="glass-strong absolute inset-y-0 left-0 flex w-[86%] max-w-xs flex-col rounded-r-2xl">
            <div className="flex items-center justify-between border-b border-[var(--rule)] px-4 py-3">
              <Link href={`/${locale}/`} className="flex items-center gap-2 font-semibold">
                <GopherMark className="h-7 w-7" />
                <span>orm</span>
              </Link>
              <button
                type="button"
                onClick={() => setOpen(false)}
                aria-label={t("close", locale)}
                className="grid h-8 w-8 place-items-center rounded-lg text-[var(--fg-muted)]"
              >
                <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round">
                  <path d="M18 6 6 18M6 6l12 12" />
                </svg>
              </button>
            </div>
            <div className="flex-1 overflow-y-auto px-3 py-4">
              <Sidebar locale={locale} onNavigate={() => setOpen(false)} />
            </div>
          </div>
        </div>
      )}
    </>
  );
}

/* ----------------------------------------------------------- code copy --- */

/**
 * Copy buttons are attached to the rendered HTML rather than rendered into it.
 *
 * The code blocks come out of the markdown pipeline as a string, so there is no
 * React tree to hang a handler on. One delegated listener on the document is
 * both the smallest thing that works and the only one that keeps working when a
 * page swaps its content on navigation.
 */
export function CodeCopy({ locale }: { locale: Locale }) {
  useEffect(() => {
    function onClick(e: MouseEvent) {
      const target = e.target as HTMLElement | null;
      const button = target?.closest<HTMLButtonElement>(".code-copy");
      if (!button) return;
      const code = button.closest<HTMLElement>(".code-block")?.dataset.code;
      if (!code) return;
      void navigator.clipboard.writeText(code).then(() => {
        button.classList.add("copied");
        button.setAttribute("data-label", t("copied", locale));
        setTimeout(() => {
          button.classList.remove("copied");
          button.removeAttribute("data-label");
        }, 1600);
      });
    }
    document.addEventListener("click", onClick);
    return () => document.removeEventListener("click", onClick);
  }, [locale]);
  return null;
}
