"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useRouter } from "next/navigation";
import { t, type Locale } from "@/lib/nav";
import { GopherSearching } from "./Gopher";

/*
  Search.

  The index is built at build time and fetched once, on the first open — so the
  bundle does not carry it and a reader who never searches never downloads it.

  Scoring is written out rather than delegated. What a docs reader types is a
  type name, a method or a fragment of SQL, and the ranking that serves that is:
  an exact phrase in a heading beats a phrase in prose, a title match beats a
  body match, and every term has to appear somewhere or the record is not a
  result at all. A fuzzy library would rank "the" highly and call it relevance.
*/

type Record_ = {
  /** Page slug, without locale. */
  s: string;
  /** Page title. */
  t: string;
  /** Heading text, empty for the page record. */
  h: string;
  /** Heading id, empty for the page record. */
  i: string;
  /** Body text of the section. */
  b: string;
};

type Hit = { rec: Record_; score: number; snippet: string };

const MAX_HITS = 12;

function normalise(s: string): string {
  return s.toLowerCase().replace(/[^\p{L}\p{N}\s._*/-]/gu, " ");
}

function scoreRecord(rec: Record_, terms: string[], phrase: string): number {
  const title = normalise(rec.t);
  const head = normalise(rec.h);
  const body = normalise(rec.b);
  const haystack = `${title} ${head} ${body}`;

  // Every term must appear. A record missing one is not a weak match, it is a
  // different subject.
  for (const term of terms) {
    if (!haystack.includes(term)) return 0;
  }

  let score = 0;
  if (phrase.length > 2) {
    if (title.includes(phrase)) score += 120;
    if (head.includes(phrase)) score += 90;
    if (body.includes(phrase)) score += 30;
  }
  for (const term of terms) {
    if (title === term) score += 100;
    else if (title.includes(term)) score += 45;
    if (head.includes(term)) score += 35;
    // A word boundary is worth more than a substring: "col" inside "column" is
    // a weaker signal than "col" standing alone.
    if (new RegExp(`\\b${term.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}`, "u").test(body)) {
      score += 12;
    } else if (body.includes(term)) {
      score += 4;
    }
  }
  // A page record outranks its own sections when the query is about the page.
  if (!rec.h) score += 8;
  return score;
}

function makeSnippet(body: string, terms: string[]): string {
  const lower = body.toLowerCase();
  let at = -1;
  for (const term of terms) {
    const i = lower.indexOf(term);
    if (i >= 0 && (at < 0 || i < at)) at = i;
  }
  if (at < 0) return body.slice(0, 150);
  const start = Math.max(0, at - 60);
  const end = Math.min(body.length, at + 110);
  return (start > 0 ? "…" : "") + body.slice(start, end).trim() + (end < body.length ? "…" : "");
}

/** Wraps the matched terms so the eye lands on them. */
function Highlight({ text, terms }: { text: string; terms: string[] }) {
  if (terms.length === 0) return <>{text}</>;
  const pattern = new RegExp(
    `(${terms.map((x) => x.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")).join("|")})`,
    "gi",
  );
  const parts = text.split(pattern);
  return (
    <>
      {parts.map((part, i) =>
        pattern.test(part) && i % 2 === 1 ? (
          <mark key={i} className="rounded bg-[var(--accent-soft)] px-0.5 text-[var(--accent)]">
            {part}
          </mark>
        ) : (
          <span key={i}>{part}</span>
        ),
      )}
    </>
  );
}

export function SearchButton({ locale }: { locale: Locale }) {
  const [open, setOpen] = useState(false);

  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setOpen((v) => !v);
      }
      if (e.key === "/" && !open) {
        const el = document.activeElement;
        const typing =
          el instanceof HTMLInputElement ||
          el instanceof HTMLTextAreaElement ||
          (el as HTMLElement | null)?.isContentEditable;
        if (!typing) {
          e.preventDefault();
          setOpen(true);
        }
      }
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open]);

  return (
    <>
      <button
        type="button"
        onClick={() => setOpen(true)}
        aria-label={t("searchLong", locale)}
        className="group flex h-9 items-center gap-2 rounded-xl border border-[var(--rule)] bg-[var(--glass)] px-3 text-sm text-[var(--fg-faint)] transition hover:border-[var(--accent)] hover:text-[var(--fg-muted)] sm:w-56 md:w-64"
      >
        <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" className="shrink-0">
          <circle cx="11" cy="11" r="7" />
          <path d="m20 20-3.5-3.5" />
        </svg>
        <span className="hidden flex-1 text-left sm:block">{t("search", locale)}</span>
        <kbd className="ml-auto hidden rounded border border-[var(--rule)] px-1.5 py-0.5 font-mono text-[0.65rem] sm:block">
          ⌘K
        </kbd>
      </button>
      {open && <SearchDialog locale={locale} onClose={() => setOpen(false)} />}
    </>
  );
}

function SearchDialog({ locale, onClose }: { locale: Locale; onClose: () => void }) {
  const router = useRouter();
  const [query, setQuery] = useState("");
  const [index, setIndex] = useState<Record_[] | null>(null);
  const [cursor, setCursor] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLUListElement>(null);

  useEffect(() => {
    let alive = true;
    fetch(`/search-${locale}.json`)
      .then((r) => r.json())
      .then((data: Record_[]) => {
        if (alive) setIndex(data);
      })
      .catch(() => setIndex([]));
    return () => {
      alive = false;
    };
  }, [locale]);

  useEffect(() => {
    inputRef.current?.focus();
    document.body.style.overflow = "hidden";
    return () => {
      document.body.style.overflow = "";
    };
  }, []);

  const { hits, terms } = useMemo(() => {
    const q = normalise(query).trim();
    if (!q || !index) return { hits: [] as Hit[], terms: [] as string[] };
    const terms = q.split(/\s+/).filter(Boolean);
    const phrase = q;
    const scored: Hit[] = [];
    for (const rec of index) {
      const score = scoreRecord(rec, terms, phrase);
      if (score > 0) {
        scored.push({ rec, score, snippet: makeSnippet(rec.b || rec.t, terms) });
      }
    }
    scored.sort((a, b) => b.score - a.score);
    return { hits: scored.slice(0, MAX_HITS), terms };
  }, [query, index]);

  useEffect(() => setCursor(0), [query]);

  const go = useCallback(
    (hit: Hit) => {
      const anchor = hit.rec.i ? `#${hit.rec.i}` : "";
      router.push(`/${locale}/docs/${hit.rec.s}/${anchor}`);
      onClose();
    },
    [locale, onClose, router],
  );

  function onKeyDown(e: React.KeyboardEvent) {
    if (e.key === "Escape") {
      e.preventDefault();
      onClose();
    } else if (e.key === "ArrowDown") {
      e.preventDefault();
      setCursor((c) => Math.min(c + 1, hits.length - 1));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setCursor((c) => Math.max(c - 1, 0));
    } else if (e.key === "Enter" && hits[cursor]) {
      e.preventDefault();
      go(hits[cursor]);
    }
  }

  useEffect(() => {
    listRef.current
      ?.querySelector(`[data-i="${cursor}"]`)
      ?.scrollIntoView({ block: "nearest" });
  }, [cursor]);

  return (
    <div
      className="fixed inset-0 z-[100] flex items-start justify-center p-3 pt-[8vh] sm:p-6 sm:pt-[12vh]"
      role="dialog"
      aria-modal="true"
      aria-label={t("searchLong", locale)}
    >
      <div className="absolute inset-0 bg-black/45 backdrop-blur-sm" onClick={onClose} aria-hidden />

      <div className="glass-strong relative flex max-h-[80vh] w-full max-w-2xl flex-col overflow-hidden rounded-2xl">
        <div className="flex items-center gap-3 border-b border-[var(--rule)] px-4">
          <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" className="shrink-0 text-[var(--fg-faint)]">
            <circle cx="11" cy="11" r="7" />
            <path d="m20 20-3.5-3.5" />
          </svg>
          <input
            ref={inputRef}
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            onKeyDown={onKeyDown}
            placeholder={t("searchPlaceholder", locale)}
            className="h-14 flex-1 bg-transparent text-base outline-none placeholder:text-[var(--fg-faint)]"
            autoComplete="off"
            autoCorrect="off"
            spellCheck={false}
            type="search"
            enterKeyHint="go"
          />
          <button
            type="button"
            onClick={onClose}
            aria-label={t("close", locale)}
            className="rounded-lg border border-[var(--rule)] px-2 py-1 font-mono text-[0.65rem] text-[var(--fg-faint)]"
          >
            ESC
          </button>
        </div>

        <div className="flex-1 overflow-y-auto overscroll-contain">
          {query && hits.length === 0 && (
            <div className="px-6 py-14 text-center">
              <GopherSearching className="mx-auto mb-4 h-24 w-24 opacity-70" />
              <p className="font-medium">{t("noResults", locale)}</p>
              <p className="mt-1 text-sm text-[var(--fg-faint)]">{t("noResultsHint", locale)}</p>
            </div>
          )}

          {!query && (
            <div className="px-6 py-14 text-center">
              <GopherSearching className="mx-auto mb-4 h-24 w-24 opacity-70" />
              <p className="font-medium">{t("startTyping", locale)}</p>
              <p className="mt-1 text-sm text-[var(--fg-faint)]">{t("startTypingHint", locale)}</p>
            </div>
          )}

          {hits.length > 0 && (
            <ul ref={listRef} className="p-2">
              {hits.map((hit, i) => (
                <li key={`${hit.rec.s}-${hit.rec.i}-${i}`} data-i={i}>
                  <button
                    type="button"
                    onClick={() => go(hit)}
                    onMouseEnter={() => setCursor(i)}
                    className={`flex w-full flex-col gap-1 rounded-xl px-3 py-2.5 text-left transition ${
                      i === cursor ? "bg-[var(--accent-soft)]" : ""
                    }`}
                  >
                    <span className="flex items-center gap-2 text-sm font-semibold">
                      <span className="truncate">
                        <Highlight text={hit.rec.h || hit.rec.t} terms={terms} />
                      </span>
                      {hit.rec.h && (
                        <span className="shrink-0 truncate text-xs font-normal text-[var(--fg-faint)]">
                          {hit.rec.t}
                        </span>
                      )}
                    </span>
                    <span className="line-clamp-2 text-xs leading-relaxed text-[var(--fg-muted)]">
                      <Highlight text={hit.snippet} terms={terms} />
                    </span>
                  </button>
                </li>
              ))}
            </ul>
          )}
        </div>

        <div className="hidden items-center gap-4 border-t border-[var(--rule)] px-4 py-2 text-[0.7rem] text-[var(--fg-faint)] sm:flex">
          <span className="flex items-center gap-1">
            <kbd className="rounded border border-[var(--rule)] px-1">↑</kbd>
            <kbd className="rounded border border-[var(--rule)] px-1">↓</kbd>
            {t("navigate", locale)}
          </span>
          <span className="flex items-center gap-1">
            <kbd className="rounded border border-[var(--rule)] px-1">↵</kbd>
            {t("select", locale)}
          </span>
          <span className="flex items-center gap-1">
            <kbd className="rounded border border-[var(--rule)] px-1">esc</kbd>
            {t("dismiss", locale)}
          </span>
        </div>
      </div>
    </div>
  );
}
