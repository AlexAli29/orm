/*
  The site's shape.

  One structure, two languages. The slug is shared so that switching language on
  a page keeps you on that page — a docs site that drops you at the index when
  you change language is a docs site nobody changes language on.
*/

export const locales = ["en", "ru"] as const;
export type Locale = (typeof locales)[number];
export const defaultLocale: Locale = "en";

export function isLocale(v: string): v is Locale {
  return (locales as readonly string[]).includes(v);
}

export type NavItem = {
  /** Path under /docs, without a leading slash. */
  slug: string;
  title: Record<Locale, string>;
};

export type NavSection = {
  title: Record<Locale, string>;
  items: NavItem[];
};

export const nav: NavSection[] = [
  {
    title: { en: "Getting started", ru: "Начало работы" },
    items: [
      { slug: "introduction", title: { en: "Introduction", ru: "Введение" } },
      { slug: "installation", title: { en: "Installation", ru: "Установка" } },
      { slug: "quickstart", title: { en: "Quickstart", ru: "Быстрый старт" } },
      { slug: "concepts", title: { en: "Core concepts", ru: "Ключевые идеи" } },
    ],
  },
  {
    title: { en: "Schema", ru: "Схема" },
    items: [
      { slug: "entities", title: { en: "Entities and tags", ru: "Сущности и теги" } },
      { slug: "types", title: { en: "Type mapping", ru: "Отображение типов" } },
      { slug: "migrations", title: { en: "Migrations", ru: "Миграции" } },
      { slug: "views", title: { en: "Views and materialized views", ru: "Представления" } },
    ],
  },
  {
    title: { en: "Querying", ru: "Запросы" },
    items: [
      { slug: "queries", title: { en: "Queries", ru: "Запросы" } },
      { slug: "predicates", title: { en: "Predicates", ru: "Предикаты" } },
      { slug: "relations", title: { en: "Relations", ru: "Связи" } },
      { slug: "projections", title: { en: "Projections", ru: "Проекции" } },
      { slug: "composition", title: { en: "Composition", ru: "Композиция" } },
      { slug: "union-all", title: { en: "UNION ALL", ru: "UNION ALL" } },
      { slug: "writing", title: { en: "Writing data", ru: "Запись данных" } },
      { slug: "transactions", title: { en: "Transactions", ru: "Транзакции" } },
    ],
  },
  {
    title: { en: "Cookbook", ru: "Рецепты" },
    items: [
      { slug: "cookbook/queries", title: { en: "Query recipes", ru: "Рецепты запросов" } },
      { slug: "cookbook/insane", title: { en: "Hard queries", ru: "Сложные запросы" } },
      { slug: "cookbook/architecture", title: { en: "Project architectures", ru: "Архитектуры проектов" } },
    ],
  },
  {
    title: { en: "Operations", ru: "Эксплуатация" },
    items: [
      { slug: "observability", title: { en: "Tracing and health", ru: "Трейсинг и здоровье" } },
      { slug: "performance", title: { en: "Performance", ru: "Производительность" } },
      { slug: "testing", title: { en: "Testing", ru: "Тестирование" } },
      { slug: "compatibility", title: { en: "Compatibility", ru: "Совместимость" } },
    ],
  },
];

/** Every slug the site publishes, in reading order. */
export function allSlugs(): string[] {
  return nav.flatMap((s) => s.items.map((i) => i.slug));
}

/** The item before and after a slug, for the footer pager. */
export function neighbours(slug: string): { prev?: NavItem; next?: NavItem } {
  const flat = nav.flatMap((s) => s.items);
  const i = flat.findIndex((x) => x.slug === slug);
  if (i < 0) return {};
  return { prev: flat[i - 1], next: flat[i + 1] };
}

export function titleFor(slug: string, locale: Locale): string {
  for (const s of nav) {
    for (const i of s.items) if (i.slug === slug) return i.title[locale];
  }
  return slug;
}

/** UI strings. Small enough to live here rather than in a translation runtime. */
export const ui = {
  search: { en: "Search", ru: "Поиск" },
  searchLong: { en: "Search the documentation", ru: "Поиск по документации" },
  searchPlaceholder: { en: "Search docs, types, examples…", ru: "Искать в документации…" },
  noResults: { en: "Nothing matched", ru: "Ничего не найдено" },
  noResultsHint: {
    en: "Try a type name, a method, or a piece of SQL.",
    ru: "Попробуйте имя типа, метод или фрагмент SQL.",
  },
  startTyping: { en: "Start typing to search", ru: "Начните вводить запрос" },
  startTypingHint: {
    en: "Every page, heading and example is indexed.",
    ru: "Проиндексированы все страницы, заголовки и примеры.",
  },
  onThisPage: { en: "On this page", ru: "На этой странице" },
  previous: { en: "Previous", ru: "Назад" },
  next: { en: "Next", ru: "Вперёд" },
  menu: { en: "Menu", ru: "Меню" },
  close: { en: "Close", ru: "Закрыть" },
  theme: { en: "Toggle theme", ru: "Сменить тему" },
  language: { en: "Language", ru: "Язык" },
  docs: { en: "Documentation", ru: "Документация" },
  getStarted: { en: "Get started", ru: "Начать" },
  copy: { en: "Copy", ru: "Копировать" },
  copied: { en: "Copied", ru: "Скопировано" },
  navigate: { en: "to navigate", ru: "навигация" },
  select: { en: "to select", ru: "выбрать" },
  dismiss: { en: "to close", ru: "закрыть" },
} as const;

export type UIKey = keyof typeof ui;
export function t(key: UIKey, locale: Locale): string {
  return ui[key][locale];
}
