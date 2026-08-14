# Definition identity for views

A view's definition is compared for three different reasons, and no single
representation answers all of them. Conflating any two is how a comparison
starts lying, so they are named apart.

## The three identities

| | what it is | portable? | committed? | answers |
|---|---|---|---|---|
| **SourceIdentity** | fingerprint of the developer's SQL | yes | yes — `orm.lock` | has the declaration changed? |
| **ServerCanonical** | `pg_get_viewdef` at apply time | **no** | never | what did *this* server make of it? |
| **ActualDefinition** | `pg_get_viewdef` now | **no** | never | what does it hold *now*? |

None of these is SQL semantic equivalence. Nothing proves that two differently
written queries mean the same thing, and nothing here parses SQL.

## Which consumer uses which

| consumer | identity | why |
|---|---|---|
| `orm.lock` | SourceIdentity | must be byte-identical on PG14 and PG18 |
| `makemigrations` change detection | SourceIdentity | runs offline, against migration state |
| `orm check` definition drift | ServerCanonical vs ActualDefinition | both from one server |
| `migrate` apply | source SQL → server; records ServerCanonical | the one command entitled to DDL |
| database-first | ActualDefinition | there is no declaration to compare against |

## How a read-only check detects a manual edit

This is the release-critical case:

```sql
declared:  SELECT id FROM users WHERE active
altered:   SELECT id FROM users WHERE NOT active
```

The columns are identical, so nothing about the relation's shape can see it.

The project's own SQL cannot be compared against `pg_get_viewdef` — PostgreSQL
returns a reconstruction of the parsed query, so the two differ even when
nothing changed. Canonicalising the project's SQL means parsing it, and a SQL
parser that is approximately right about PostgreSQL is worse than none: it
would silently call two different definitions equal. Asking the server to
canonicalise it means creating the view, and `orm check` is read-only —
including transient DDL that rolls back, which is still DDL and still needs
privileges a check should not have.

What is left is to ask the server **once, at the only moment DDL is
legitimate**. When `migrate` runs `CREATE VIEW`, it reads back what PostgreSQL
made of the definition and records it in `public.orm_schema_views`, in the
database it applied to, inside the migration's own transaction. A later check
reads that recording and reads `pg_get_viewdef` now, and compares those two.

Both come from one server, so the deparser's version cancels out: formatting and
comments are already gone, and a changed predicate is not.

The recording is per-database and never committed, because deparsed text differs
between majors — PostgreSQL 16 stopped qualifying columns it does not need to,
so one unchanged view reads `SELECT xt.id FROM xt` on 15 and `SELECT id FROM xt`
on 16. Putting that in `orm.lock` would make an identical project produce
different bytes depending on which server it was pointed at.

The read-only guarantee is a type, not a rule: a check is handed a `ViewReader`,
which has no `Exec`. There is nothing it could call to write.

## Formatting

**Reformatting a declaration changes its SourceIdentity.** This is a decision,
not an oversight.

Making it formatting-independent needs a tokenizer that knows a space inside a
string literal is data and a space between keywords is not — that is a SQL
parser, and an approximate one would call two different definitions equal.

The consequence is small: reindenting a definition changes the lock and produces
a migration whose SQL PostgreSQL parses to the query it already had. That is a
harmless statement.

Formatting *is* invisible to drift detection, because that comparison happens
between two deparsed texts from one server. The two halves give different
answers to "did the formatting change matter", and both answers are correct for
the question being asked.

## Dependency reconciliation

Desired dependencies are the direct relation dependencies a declaration states
with `//orm:depends-on`. Actual dependencies are the direct relation
dependencies PostgreSQL reports for the stored query, after the foundation's
filtering: relations only, self excluded, extension-owned objects excluded.

Both sides are **direct** and neither is a closure. A view reading a view
depends on that view, not on what it reads — flattening either side would make a
two-level graph compare as a one-level one, and any ordering built from it would
be right only by accident.

Identity is always schema **and** name. `public.users` and `archive.users` are
different relations, and an unqualified comparison is how a migration ends up
ordered against the wrong table.

Both directions are errors:

| | code |
|---|---|
| the relation reads something no declaration mentions | `E033` |
| a declared dependency the relation does not have | `E034` |

A dependency set is not documentation — it is the input to migration ordering,
and an edge the graph does not have is as wrong as one it is missing. Reporting
only one direction would leave the graph half-checked.

## Adoption: what a planner may not assume

A relation that exists with no recorded provenance reports `W031`. Before Stage
G, this is the frozen precondition:

> **An existing relation with no provenance is not a safe replacement target.**

Matching columns and a matching kind do **not** imply a matching body. A
predicate can differ with every column identical — which is precisely what the
recording exists to detect. A planner must never write provenance for an
existing relation on the strength of its shape.

Adopting such a relation is an explicit operation, not an inference. It is not
implemented; until it is, `W031` stands and the body is unverified.

## Metadata a managed declaration cannot express

`W035` reports view options present in the database that managed mode has no
syntax for — `security_barrier`, `security_invoker`, a check option. These are
not decoration: `security_invoker` decides whose privileges the base tables are
read with. Silence would state that something had been checked which cannot even
be represented, so the option is named and the decision left with the reader.
