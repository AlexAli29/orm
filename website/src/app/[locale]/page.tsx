import { SoftwareSchema } from '@/components/StructuredData';
import Link from 'next/link';
import { codeToHtml } from 'shiki';
import { locales, isLocale, type Locale } from '@/lib/nav';
import { GopherWithDatabase, GopherFace } from '@/components/Gopher';

export function generateStaticParams() {
  return locales.map((locale) => ({ locale }));
}

const copy = {
  en: {
    tagline: 'A schema-reconciling PostgreSQL mapper for Go',
    thesis: [
      'You own your structs.',
      'PostgreSQL owns your schema.',
      'The generator proves they agree.',
    ],
    lede: 'Neither representation is generated from the other. You write the structs; migrations own the schema. The orm command introspects both, reports every place they disagree, and generates typed metadata only from a mapping it proved.',
    start: 'Get started',
    browse: 'Browse the cookbook',
    featuresTitle: 'What the compiler catches',
    features: [
      {
        title: 'Types come from the catalog',
        body: "A column's Go type is what PostgreSQL says it is. A text column has Like and an integer does not; a NOT NULL column has no IsNull. None of it is a convention you have to remember.",
      },
      {
        title: 'Nullability is a property of the query',
        body: 'A NOT NULL column read through an outer join can still be NULL, so it widens. A select list that ignores that is refused at build time, not discovered as a scan error on the row that happened not to match.',
      },
      {
        title: 'Relations are asked for, never guessed',
        body: 'There is no lazy loading, so a loop over a slice cannot become a query per row. Loading is breadth-first and batched: the statement count follows the shape of the tree you asked for, never the number of rows in it.',
      },
      {
        title: 'Migrations are planned and proved',
        body: 'Declarations are diffed against the schema the migrations describe. Destructive changes are gated, materialized views have a safe lifecycle, and the artifacts are portable across every supported major.',
      },
      {
        title: 'One statement, always',
        body: 'Composition, CTEs, derived tables and UNION ALL compile through one AST and one writer, with one global placeholder namespace. Nothing is assembled from strings and nothing is stitched together in Go.',
      },
      {
        title: "PostgreSQL's own types survive",
        body: 'Ranges keep their bound model, intervals keep months and days apart, and uuid, jsonb, arrays and PostGIS geometry stay themselves rather than being reduced to something Go already had.',
      },
    ],
    proofTitle: 'Proved, not asserted',
    proofBody:
      'Every frozen milestone carries a mutation campaign: the code is deliberately broken and the suite has to notice. The compatibility matrix refuses to run against fewer than the five PostgreSQL majors the project claims.',
    stats: [
      { n: '14–18', l: 'PostgreSQL majors' },
      { n: '56', l: 'mutation classes caught' },
      { n: '0', l: 'survivors' },
    ],
    sampleTitle: 'What it reads like',
  },
  ru: {
    tagline: 'PostgreSQL-маппер для Go, который сверяет схему',
    thesis: [
      'Структуры — ваши.',
      'Схема — PostgreSQL.',
      'Генератор доказывает, что они совпадают.',
    ],
    lede: 'Ни одно представление не порождается из другого. Структуры пишете вы, схемой владеют миграции. Команда orm читает и то и другое, показывает каждое расхождение и генерирует типизированные метаданные только из отображения, которое смогла доказать.',
    start: 'Начать',
    browse: 'Открыть рецепты',
    featuresTitle: 'Что ловит компилятор',
    features: [
      {
        title: 'Типы берутся из каталога',
        body: 'Go-тип колонки — это то, что говорит PostgreSQL. У текстовой колонки есть Like, у целочисленной нет; у NOT NULL колонки нет IsNull. Ничего из этого не нужно держать в голове.',
      },
      {
        title: 'Nullability — свойство запроса',
        body: 'Колонка NOT NULL, прочитанная через outer join, всё равно может быть NULL, поэтому тип расширяется. Список выборки, который это игнорирует, отвергается при сборке, а не всплывает ошибкой сканирования на строке без совпадения.',
      },
      {
        title: 'Связи запрашиваются явно',
        body: 'Ленивой загрузки нет, поэтому цикл по срезу не может превратиться в запрос на строку. Загрузка идёт в ширину и пакетами: число запросов зависит от формы дерева, а не от количества строк в нём.',
      },
      {
        title: 'Миграции планируются и доказываются',
        body: 'Декларации сравниваются со схемой, которую описывают миграции. Разрушающие изменения проходят через шлюз, у материализованных представлений безопасный жизненный цикл, а артефакты переносимы между всеми поддерживаемыми мажорами.',
      },
      {
        title: 'Всегда один запрос',
        body: 'Композиция, CTE, производные таблицы и UNION ALL компилируются одним AST и одним писателем, с единым пространством плейсхолдеров. Ничего не собирается из строк и не склеивается в Go.',
      },
      {
        title: 'Типы PostgreSQL остаются собой',
        body: 'Диапазоны сохраняют модель границ, интервалы держат месяцы и дни раздельно, а uuid, jsonb, массивы и геометрия PostGIS остаются собой, а не сводятся к тому, что уже есть в Go.',
      },
    ],
    proofTitle: 'Доказано, а не заявлено',
    proofBody:
      'За каждой замороженной вехой стоит мутационная кампания: код намеренно ломают, и набор тестов обязан это заметить. Матрица совместимости отказывается запускаться меньше чем на пяти мажорах PostgreSQL, которые заявляет проект.',
    stats: [
      { n: '14–18', l: 'мажоры PostgreSQL' },
      { n: '56', l: 'пойманных мутаций' },
      { n: '0', l: 'выживших' },
    ],
    sampleTitle: 'Как это читается',
  },
} as const;

const SAMPLE = `// The type parameters are the point: a Predicate[User] cannot
// reach a query over Post, and a NOT NULL column has no IsNull.
users, err := db.Users.Query().
    Where(Users.Email.ILike("%@example.com")).
    Where(Users.CreatedAt.Gte(cutoff)).
    With(Users.Orders.OrderBy(Orders.Placed.Desc()).Limit(5)).
    OrderBy(Users.CreatedAt.Desc()).
    Limit(50).
    All(ctx)`;

export default async function Landing({
  params,
}: {
  params: Promise<{ locale: string }>;
}) {
  const { locale: raw } = await params;
  const locale: Locale = isLocale(raw) ? raw : 'en';
  const c = copy[locale];

  const sample = await codeToHtml(SAMPLE, {
    lang: 'go',
    themes: { light: 'github-light', dark: 'github-dark' },
    defaultColor: false,
  });

  return (
    <>
      <SoftwareSchema locale={locale} />
      <main id="main">
        {/* hero */}
        <section className="mx-auto max-w-[100rem] px-4 pb-16 pt-14 sm:px-6 sm:pt-20">
          <div className="grid items-center gap-10 lg:grid-cols-[1.15fr_0.85fr]">
            <div>
              <p className="mb-5 inline-flex items-center gap-2 rounded-full border border-[var(--rule)] bg-[var(--glass)] px-3 py-1 text-xs font-medium text-[var(--fg-muted)]">
                <span className="h-1.5 w-1.5 rounded-full bg-[var(--color-go-blue)]" />
                {c.tagline}
              </p>

              <h1 className="text-4xl font-bold leading-[1.1] tracking-tight sm:text-5xl lg:text-6xl">
                {c.thesis.map((line, i) => (
                  <span
                    key={i}
                    className={
                      i === 2
                        ? 'block bg-gradient-to-r from-[var(--color-go-blue)] to-[var(--color-go-light)] bg-clip-text text-transparent'
                        : 'block'
                    }
                  >
                    {line}
                  </span>
                ))}
              </h1>

              <p className="mt-6 max-w-2xl text-lg leading-relaxed text-[var(--fg-muted)]">
                {c.lede}
              </p>

              <div className="mt-8 flex flex-wrap gap-3">
                <Link
                  href={`/${locale}/docs/introduction/`}
                  className="rounded-xl bg-[var(--color-go-blue)] px-5 py-3 font-semibold text-white shadow-lg shadow-[var(--color-go-blue)]/25 transition hover:brightness-110"
                >
                  {c.start}
                </Link>
                <Link
                  href={`/${locale}/docs/cookbook/queries/`}
                  className="glass rounded-xl px-5 py-3 font-semibold transition hover:border-[var(--accent)]"
                >
                  {c.browse}
                </Link>
              </div>
            </div>

            <div className="relative mx-auto w-full max-w-sm lg:max-w-none">
              <div className="glass rounded-3xl p-8">
                <GopherWithDatabase
                  className="mx-auto h-56 w-auto drop-shadow-xl sm:h-64"
                  label={
                    locale === 'ru'
                      ? 'Гофер с базой данных'
                      : 'A gopher holding a database'
                  }
                />
              </div>
            </div>
          </div>
        </section>

        {/* sample */}
        <section className="mx-auto max-w-[100rem] px-4 pb-16 sm:px-6">
          <div className="glass mx-auto max-w-4xl overflow-hidden rounded-2xl">
            <div className="flex items-center gap-2 border-b border-[var(--rule)] px-4 py-2.5">
              <span className="h-2.5 w-2.5 rounded-full bg-[var(--color-go-pink)]" />
              <span className="h-2.5 w-2.5 rounded-full bg-[var(--color-go-yellow)]" />
              <span className="h-2.5 w-2.5 rounded-full bg-[var(--color-go-blue)]" />
              <span className="ml-2 font-mono text-xs text-[var(--fg-faint)]">
                {c.sampleTitle}
              </span>
            </div>
            <div
              className="prose max-w-none [&_pre]:m-0 [&_pre]:rounded-none [&_pre]:border-0 [&_pre]:shadow-none"
              dangerouslySetInnerHTML={{ __html: sample }}
            />
          </div>
        </section>

        {/* features */}
        <section className="mx-auto max-w-[100rem] px-4 pb-16 sm:px-6">
          <h2 className="mb-8 text-center text-2xl font-bold tracking-tight sm:text-3xl">
            {c.featuresTitle}
          </h2>
          <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
            {c.features.map((f) => (
              <div
                key={f.title}
                className="glass rounded-2xl p-6 transition hover:border-[var(--accent)]"
              >
                <h3 className="mb-2 font-semibold">{f.title}</h3>
                <p className="text-sm leading-relaxed text-[var(--fg-muted)]">
                  {f.body}
                </p>
              </div>
            ))}
          </div>
        </section>

        {/* proof */}
        <section className="mx-auto max-w-[100rem] px-4 pb-24 sm:px-6">
          <div className="glass grid items-center gap-8 rounded-3xl p-8 sm:p-12 lg:grid-cols-[auto_1fr]">
            <GopherFace className="mx-auto h-28 w-28" />
            <div>
              <h2 className="text-2xl font-bold tracking-tight">
                {c.proofTitle}
              </h2>
              <p className="mt-3 max-w-2xl leading-relaxed text-[var(--fg-muted)]">
                {c.proofBody}
              </p>
              <dl className="mt-6 flex flex-wrap gap-8">
                {c.stats.map((s) => (
                  <div key={s.l}>
                    <dt className="text-3xl font-bold text-[var(--accent)]">
                      {s.n}
                    </dt>
                    <dd className="text-sm text-[var(--fg-faint)]">{s.l}</dd>
                  </div>
                ))}
              </dl>
            </div>
          </div>
        </section>

        <footer className="border-t border-[var(--rule)] py-10">
          <div className="mx-auto max-w-[100rem] px-4 text-center text-sm text-[var(--fg-faint)] sm:px-6">
            {/*
            For coding agents. The index and the whole-docs file are plain text,
            and the API manifest beside them is the generated list of every
            exported symbol — which is the thing an agent most needs and is
            least able to guess.
          */}
            <p className="mb-4">
              {locale === 'ru' ? 'Для агентов: ' : 'For agents: '}
              <a
                className="underline hover:text-[var(--fg)]"
                href={`/llms${locale === 'ru' ? '.ru' : ''}.txt`}
              >
                llms.txt
              </a>
              {' · '}
              <a
                className="underline hover:text-[var(--fg)]"
                href={`/llms-full${locale === 'ru' ? '.ru' : ''}.txt`}
              >
                {locale === 'ru' ? 'вся документация' : 'all docs'}
              </a>
              {' · '}
              <a
                className="underline hover:text-[var(--fg)]"
                href="/api/orm.txt"
              >
                {locale === 'ru' ? 'манифест API' : 'API manifest'}
              </a>
            </p>
            <p>
              {locale === 'ru'
                ? 'Гоферы нарисованы для этого сайта и вдохновлены гофером Go — оригинальный персонаж Рене Френч, лицензия CC BY 3.0.'
                : "The gophers here were drawn for this site, in the spirit of the Go gopher — the original character is Renée French's, CC BY 3.0."}
            </p>
          </div>
        </footer>
      </main>
    </>
  );
}
