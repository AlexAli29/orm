# Using these docs with an agent

> Plain-text documentation and a generated symbol list, for coding assistants.

Source: https://ormgo.vercel.app/en/docs/agents/
Symbols: https://ormgo.vercel.app/api/orm.txt — the generated list of every exported name.

---
## What is here

Everything on this site is also published as plain text, because a coding
assistant gets one URL and whatever is behind it — and what is behind a page is
otherwise a React application.

| URL | What it is |
| --- | --- |
| [`/llms.txt`](/llms.txt) | The index. Every page, one line each, with its description. |
| [`/llms-full.txt`](/llms-full.txt) | All of the English documentation inline, in navigation order. One fetch. |
| [`/llms-full.ru.txt`](/llms-full.ru.txt) | The same in Russian. |
| [`/api/orm.txt`](/api/orm.txt) | Every exported symbol in the library. |
| `…/<page>.md` | Any page as its own source markdown. |

The last one is a suffix, not a separate site. Add `.md` to a page's URL and you
get the markdown that page was rendered from:

```text
https://ormgo.vercel.app/en/docs/projections/      the page
https://ormgo.vercel.app/en/docs/projections.md    its source
```

Every rendered page also advertises its own markdown in a `rel="alternate"`
link, so a tool that looks for one does not have to be told the convention.

## Start with the symbol list

If you read one file, read [`/api/orm.txt`](/api/orm.txt).

Every line of code written against a library is a guess about which names exist,
and the guesses that cost you an afternoon are the plausible ones. `orm.Returning`
reads like it should be a function — it is a generic type. `EqCol` reads like the
obvious way to compare two columns. `Users.Table()` reads like something every
ORM has. None of them exist here, and nothing about the shape of a query makes
that visible until the compiler says so.

`orm.txt` is generated from the packages themselves by the same tool CI uses to
diff the public API, so it is exhaustive rather than representative:

```text
package github.com/AlexAli29/orm
const BoundEmpty BoundKind = 0
func Project12[E any, T1 any, ...](...)
method (*ViewRepo) Query() *Query[E]
```

The rule it supports is short enough to give an assistant directly: **if a name
is not in `orm.txt`, it does not exist.** No amount of plausibility changes that,
and checking is a search rather than a build.

The two smaller manifests cover the packages that are not the ORM itself —
[`/api/ormtest-postgres.txt`](/api/ormtest-postgres.txt) for the test helpers and
[`/api/ormotel.txt`](/api/ormotel.txt) for the OpenTelemetry integration.

## Pointing a tool at it

Most assistants take a file of project instructions — `CLAUDE.md`, `AGENTS.md`,
a Cursor rule. Three lines in one of those does the job:

```text
This project uses github.com/AlexAli29/orm.

Docs:    https://ormgo.vercel.app/llms.txt
Symbols: https://ormgo.vercel.app/api/orm.txt

Before using any orm.* name, confirm it appears in api/orm.txt. If it is not
there it does not exist, however plausible it looks. Do not guess at method
names on generated columns — the generator decides them, and the manifest
lists them.
```

Pointing at `llms.txt` rather than `llms-full.txt` is usually the better default:
the index is small, and it lets the assistant fetch the one page it needs instead
of carrying the whole manual. Reach for `llms-full.txt` when the tool cannot
follow links, or when you would rather pay once for the lot.

## What is generated, and when

Nothing on those URLs is written by hand. The page list is the site's own
navigation, the prose is the same markdown the pages render from, and the
manifests are copied from the repository where CI regenerates and diffs them on
every change to the public API.

That has a consequence worth stating plainly: the plain-text docs cannot fall
behind the site, because they are built from it in the same step. They can only
be as current as the deploy, which is the same guarantee the HTML has.

## Worked examples

### Checking a name before using it

The question an assistant should ask before writing `orm.Something`:

```text
$ curl -s https://ormgo.vercel.app/api/orm.txt | grep '^type Returning'
type Returning[E any, R any] struct
```

A type, and one taking two parameters — so `orm.Returning(Summaries)` is not a
call that exists, and the way to reach it is `orm.UpdateReturning(upd, shape)`.
The manifest said so before the compiler did.

### Fetching one page instead of the manual

An assistant asked to add a materialized view needs one page, not thirty:

```text
https://ormgo.vercel.app/en/docs/views.md
```

The file opens with the page's title, description, its canonical URL and a
pointer back to the symbol list, then the markdown — a twentieth of what the
whole manual would have cost.

### Giving a reviewer the whole thing

A review pass over a large diff wants the manual in context and does not want
thirty fetches:

```text
https://ormgo.vercel.app/llms-full.txt
```

One file, every English page, in navigation order.

### Working in Russian

The Russian documentation is a translation of the prose, not of the code — every
example is byte-identical across the two languages, and a test enforces it. An
assistant reading `/llms-full.ru.txt` gets Russian explanations of the same Go
that appears in the English pages:

```text
https://ormgo.vercel.app/llms.ru.txt
https://ormgo.vercel.app/llms-full.ru.txt
```
