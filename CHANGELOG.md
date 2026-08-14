# Changelog

This project follows [semantic versioning](https://semver.org). Before v1.0 the
exported API may change between minor versions; it will not change without an
entry here.

## v0.1.0 — 2026-08-14

### UNION ALL — in progress

Not finished, and not frozen. Five pieces are in: the compound node in the
compiler, the public builder with the rule that decides whether two branches may
be combined, a compound as a row source and as an ordinary CTE body, a compound
in a value position, and ordering a compound by its own output columns. Writes,
recursive CTEs, `EXISTS`, and set operators other than `UNION ALL` are not done.

**Added** — an additive change to the public API; nothing was removed or
changed.

- `orm.UnionAll(branches...)` concatenates two or more branches, keeping
  duplicates, and returns `*orm.UnionQuery[R]` with `Using`, `Limit`, `Offset`,
  `SQL`, `All`, `One` and `Rows`. The branches are variadic rather than a pair:
  `A UNION ALL B UNION ALL C` is one operation over three inputs, not a tree of
  pairs someone associated.
- `orm.Branch[R]` names what can be a branch. It is closed by an unexported
  method, so only this package's builders satisfy it — `*Query[E]`,
  `*SelectQuery[E, R]`, `*ComposedQuery[R]` and `*UnionQuery[R]` itself, which
  is what makes a validated union usable as a branch of another.

**How branches are checked.** Two branches must produce the same Go result type,
which the compiler enforces, *and* the same result shape, which the builder
checks: the number of columns, and each column's Go destination type and
nullability, positionally. `R` alone is not the rule — two projections can build
the same struct out of different columns, and a rule reading only `R` would
accept a branch feeding a nullable column into a destination the other branch
proved non-null.

The shape is built where the facts already are, at construction time: a
projection's from the same values its select list is built from, an entity's from
the generated descriptor rather than from `E`, so two entities of one Go type
whose catalogs disagree are different results. Nothing is read from a rendered
statement or from a row that came back, and the row-scanning path is unchanged
and still reflection-free.

Mismatches are refused rather than reconciled. `int32` against `int64`, `T`
against `*T`, nullable against non-nullable: PostgreSQL could coerce some of
those, and coercing them here would mean choosing a destination type the caller
did not write. What is left to PostgreSQL is whether two SQL expressions in a
slot have compatible SQL types — the AST does not carry SQL types and this
milestone does not invent a shadow type system to guess them.

**A set operation is a row source.** `orm.Sub` makes one a derived table and
`orm.CTE` makes one the body of an ordinary CTE, so `SELECT ... FROM (a UNION
ALL b) AS u` and `WITH x AS (a UNION ALL b) SELECT ... FROM x` are both
expressible. Every join that takes a source takes one of these, a compound
source can itself contain a compound, and a union that becomes a source is
unchanged by it — the same value is still a branch of another union, carrying
the shape its branches were validated against.

The output column names are the first branch's, which is PostgreSQL's rule for a
compound and not a choice made here. They are read from the result shape, so a
union whose first branch does not name its columns is refused with a diagnostic
saying which branch to fix.

**Changed** — a breaking signature change, and the reason for it.

- `orm.Sub` and `orm.CTE` now take `orm.SourceTerm` instead of `orm.Term`.

  `Term`'s contract is to produce a plain `SELECT`, which `EXISTS` and a
  recursive CTE genuinely need because they rewrite and compare select
  internals. A set operation is not a `SELECT`, so widening `Term` would have
  produced values that satisfy it and fail when something reads them.
  `SourceTerm` is a separate closed interface returning the statement to nest
  and the names of the columns it provides — the shape `WriteTerm` already had.
  What can be a source and what is a `SELECT` are now separate claims.

  Every call that could ever have worked still compiles: `SourceTerm` is
  satisfied by `ComposedQuery`, `SelectQuery` and `UnionQuery`. What stops
  compiling is `orm.Sub("u", repo.Query())` — an entity query selects its
  descriptor's columns and names none of them, so a derived table over it had
  nothing for its columns to be addressed by. It compiled and failed when the
  statement was built; now it does not compile.

  `RecursiveCTE` and `RecursiveCTEUnion` still take `Term`, which is what makes
  a compound anchor a compile error rather than something to validate around.

**A set operation is a value subquery.** `orm.InSub`, `orm.NotInSub` and
`orm.Scalar` take one, so `x IN (a UNION ALL b)` and `SELECT (a UNION ALL b)`
are both expressible. Compounds nest in either position, and the parameters of
every branch and of the query around them are numbered across one statement.

**Compile-time validation added.** How many columns a value subquery returns is
now checked when the query is built, from construction-time metadata rather than
from the tree or from PostgreSQL:

- A membership test compares as many columns as the expression on its left,
  which is one. A wider right-hand side is refused with both numbers named.
- A scalar subquery reads exactly one column. `Scalar` did not check this
  before; the writer refused more than one column when the statement was
  rendered, and said nothing about a compound whose branches disagree.
- For a set operation the number comes from the result shape its branches were
  validated against — the only thing that speaks for the whole operation, since
  a compound has no select list of its own and one branch's would be that
  branch's word for it.

How many *rows* a scalar subquery returns is unchanged and remains PostgreSQL's:
one column and two rows is a well-formed scalar subquery that fails at run time
in some positions and not in others.

**Changed** — a breaking signature change, and the reason for it.

- `orm.InSub`, `orm.NotInSub` and `orm.Scalar` now take `orm.ValueTerm` instead
  of `orm.Term`, for the same reason `Sub` and `CTE` took `SourceTerm`: `Term`
  produces a plain `SELECT`, and a set operation is not one.

  `ValueTerm` is a third closed capability and deliberately not `SourceTerm`. A
  source's columns are addressed by name, so a source term declares them; a
  value subquery's columns are not addressed at all, so requiring names would
  refuse membership tests PostgreSQL accepts. One union can be a valid value
  subquery and an invalid source, and the two interfaces say so separately.

  Every call that could ever have worked still compiles: `ValueTerm` is
  satisfied by `Query`, `SelectQuery`, `ComposedQuery` and `UnionQuery` — every
  read query this package builds, because every one of them is a valid
  subquery. Whether a particular one fits a particular position is decided by
  its arity, not by its kind. A write is not one; reading the rows a write
  touched is what `WritingCTE` is for.

**A set operation can be ordered by its own output columns.** `UnionQuery.OrderBy`
takes terms built from output declarations — the same values `Ref` takes — so a
union that is both ordered and selected from names its columns once:

```go
thingID := orm.Named("thing_id", orm.Of(Users.ID))
orm.UnionAll(fromUsers, fromArchive).OrderBy(thingID.Desc()).Limit(10)
```

The clause belongs to the operation, not to its last branch, so a limit now takes
the first rows of an ordering rather than some rows. An ordered union is still a
branch, a source, a CTE body and a value subquery; nested, it is parenthesised,
so its ordering and limit stay its own.

**Compile-time validation added.** A compound's `ORDER BY` accepts an output
column name and nothing else — a qualified reference is refused with
`missing FROM-clause entry`, and *any* expression, even one over an output name,
with `invalid UNION/INTERSECT/EXCEPT ORDER BY clause`. So:

- The term is an `orm.OutputOrder`, a name and a direction, with nowhere in it
  to put the expression PostgreSQL would reject. It is a different type from
  `orm.Order` and the two do not substitute in either direction.
- Which names are allowed comes from the result shape. A compound's output names
  are its first branch's, so a name only a later branch declared is refused here
  rather than by the server, and the diagnostic lists the ones there are.
- A result cannot name one column twice. That is decided where a shape is built
  rather than where one is used, so every consumer of the names inherits it —
  ordering, a derived table, a CTE body, a value subquery, a branch of another
  union. It was previously decided per consumer, and one of four had not asked.

  This forecloses something real: PostgreSQL permits duplicate output names as
  long as nothing refers to them, so a result described by a shape can no longer
  be `SELECT id AS k, name AS k FROM t`. `Sub` and `CTE` had already made that
  decision; it is now made once. Reading such a result positionally is
  untouched — an entity query selects its descriptor's columns and needs no
  shape to do it.
- The names have to be declared. This package does not model the names
  PostgreSQL derives for itself from a bare column, so a branch whose columns are
  not aliased has nothing to order by and says so.

**Added** — additive; no existing signature changed.

- `orm.OutputOrder`, `(orm.Out).Asc`, `(orm.Out).Desc`, `(*orm.UnionQuery).OrderBy`.

**What a branch may carry.** An adversarial audit found three statements the
builders would accept and PostgreSQL would not, or would read differently from
how they were written. All three are refused now.

- **A locking clause in a branch is refused.** PostgreSQL does not allow
  `FOR UPDATE` — or any locking strength — anywhere in a set operation, and
  parenthesising the branch does not help: a concatenation has no table rows to
  lock. It is refused where the branch is handed over, not by the server.
- **A branch carrying its own `WITH` is parenthesised.** Written bare in the
  first branch, PostgreSQL accepted it and declared the item for the *whole*
  compound — evaluated once for the operation and visible to every branch, which
  is not what the caller wrote. In a later branch the same omission was a syntax
  error.
- **A named query selected from but never declared is refused.** The reference
  rendered as a bare identifier, so the statement named a relation that did not
  exist. This predates set operations — a plain composed query did it too — and
  is fixed here because the two defects concealed each other: with the
  declaration hoisted, the undeclared reference resolved and the statement ran.

Together the last two made this package's own claim about branches false. A
branch could read a named query another branch declared; it cannot now.

**Added.** `(*orm.UnionQuery).With` declares named queries for the whole
operation — in front of the compound, evaluated once, visible to every branch.
That is the way to share a CTE between branches, and it exists because
parenthesising a branch's own `WITH` closed the accident that used to provide
it. Without it, closing the accident would have removed the capability.

Two smaller corrections came with them. A compound nested past the depth limit
reported the one useful sentence behind one repetition of `branch 1: ` per level;
it says it once. And the documentation for `Branch`, `SourceTerm` and `ValueTerm`
said a closed interface cannot be *satisfied* outside the package — embedding one
in a struct satisfies it with nothing behind it, and calling the method panics,
which is Go's rule for every embedded nil and true of every interface anywhere.
The comments now say what closing does buy: it cannot be implemented.

**Still not offered.** A compound is not yet a recursive CTE's anchor, an
`EXISTS` probe, or the source of a write — the first two rewrite or compare
select internals, which only a `SELECT` can answer.

Ordering by an ordinal is not offered either, and it is not equivalent to
ordering by name: `ORDER BY 1` works on a compound whose columns are not named,
which is exactly the case ordering by name refuses. So this is a gap rather than
a redundancy. It is left out because an ordinal is positional — it silently means
a different column when the select list changes — and because the shape has no
typed handle for "the first column" to hang it on. If it is wanted, the case for
it is a union nobody wants to alias, not a shorter way to say the same thing.

### Licensed

The repository has a `LICENSE` file: **MIT**. Until now it had none, which meant
the default — exclusive copyright — and `go get` granted nobody the right to use,
copy or modify it whatever the README implied. This is the last of the governance
blockers `internal/tools/releasecheck` reports that is not a tag-time step.

### v1 hardening and public contract freeze (M16)

The point of this milestone is that the project stops behaving like a
pre-release codebase. No features were added.

**The public API is now a file, not a number.** Earlier milestones tracked an
export count, which does not define an API: remove one function, add another,
and the count is unchanged while every caller breaks. [`api/`](api/) holds a
canonical structural manifest generated from `go/types` — packages, constants
and their values, signatures, type parameters *and their constraints*, struct
fields and tags, interface method sets, methods with receivers, and the shape of
any type an unexported package exposes through the public surface. CI
regenerates and diffs it, so every change to the contract appears in review as a
diff to a committed file. Eleven deliberate breaking changes — a removed
function, a renamed method, `int64`→`int`, a changed parameter, a changed
result, a pointer receiver made a value receiver, a tightened constraint, a
removed field, a method added to a consumer-implemented interface — are tested
against the tool itself.

### Removed

Accidental public API, removed at the last point where removing it is cheap.

- **The whole generator pipeline is no longer public.** `gen`, `gen/config`,
  `gen/desired`, `gen/emit`, `gen/goscan`, `gen/lock`, `gen/managed`,
  `gen/migrate`, `gen/model`, `gen/pgintro`, `gen/reconcile` and `gen/schema`
  moved to `internal/gen/`. No module outside this repository imported any of
  them; they were public because nobody had said otherwise. The public package
  count went from 24 to 12 and the manifest from 2,897 lines to 1,894.
  `gen/diag` deliberately stays public: diagnostic codes are machine-observable
  API.
- **`orm.Source`'s ten exported fields.** `Source` is reachable through a type
  alias that generated code and extensions both use, so `Kind`, `Schema`,
  `Table`, `Alias`, `AliasErr`, `Sub`, `CTEName`, `Outputs`, `Recursive` and
  `Materialized` were a permanently public, directly mutable piece of the query
  compiler — including one field holding the whole sub-statement AST and one
  holding an error a caller could forge. Nothing outside `internal/expr` read
  any of them. They are replaced by a named method set: `Relation`, `AliasName`,
  `Name`, `Err`, `IsTable`, `IsCTE`, `IsDerived`, `HasDefinition`,
  `SetRecursive`, `SetMaterialized`.

### Added

- [`docs/compatibility.md`](docs/compatibility.md) — what is public, what counts
  as breaking, what is additive, the deprecation process, and the rule that
  error strings are not API.
- [`docs/support.md`](docs/support.md) — the Go and PostgreSQL support policy,
  with every claimed version backed by a CI job.
- [`SECURITY.md`](SECURITY.md) — supported release lines, what counts as a
  vulnerability and what does not, and what to include in a report. The private
  reporting channel is marked as an owner decision rather than invented.
- `E024` now has a documented registry contract alongside every other code.
- CI: an API-manifest job, a diagnostic-register job, a documentation job, a
  minimum-Go job pinned with `GOTOOLCHAIN=local`, and a current-stable-Go job.

### Changed

- **The PostgreSQL support matrix is now every major upstream supports: 14, 15,
  16, 17 and 18**, all five run in CI. It previously tested 14 and 17 and said
  nothing about the three versions in between. PostgreSQL 18 passed with no code
  changes. PostgreSQL 14 is supported until its upstream end of life on
  12 November 2026 and will be dropped in the first minor release after that —
  a decision recorded rather than left to drift.
- **Go: minimum 1.24, tested on 1.24 and current stable.** The floor is
  deliberately below the two releases Go upstream supports, because a library
  that demands the newest toolchain excludes users for the maintainer's
  convenience. It is verified against a real 1.24 toolchain, not against the
  `go` directive alone.
- Every exported symbol in every public package now carries documentation that
  says something its name does not — 1,618 of 1,618, enforced by a linter that
  rejects `// Foo is Foo`.


### Production examples and the CI that checks them (M15.5, M15.6)

Two runnable services, each a separate Go module, each tested against a real
PostgreSQL started by the testing toolkit.

[examples/production](examples/production) — a shared domain of users, projects
and tasks behind four routers (net/http as the canonical one, plus chi, Gin and
Fiber), transactions owned by the service through `orm.RunTx`, errors translated
at the boundary on SQLSTATE, three health endpoints, a lifecycle package whose
shutdown order is proved rather than asserted, and an observability package that
fans one tracer out to `log/slog` and OpenTelemetry.

[examples/hexagonal](examples/hexagonal) — the same kind of application as ports
and adapters. The core declares what it needs, the adapters implement it, and
`boundary_test.go` reads the real import graph out of `go list -json` to state
the rules: the core reaches neither the ORM nor pgx nor `net/http` — transitively,
not just directly — and exactly two packages import the ORM.

**The module paths are outside the ORM's on purpose.** They are
`example.com/production` and `example.com/hexagonal`, because Go's `internal/`
rule is lexical rather than modular: a module named
`github.com/AlexAli29/orm/examples/production` would have been *permitted* to
import `github.com/AlexAli29/orm/internal/...`, separate `go.mod` and all. This
was found by a CI attack that broke each check on purpose and watched what
still passed.

Three CI jobs were added — a database-free compile job over every example
module, an integration job that runs them against PostgreSQL 14 and 17 through
Testcontainers, and a generation job that applies the committed migrations to an
empty database and checks that regenerating produces no diff. The two
"internals are unreachable" probes now match the compiler's *reason* rather than
only its exit code, because a build that failed for an unrelated reason kept
them green while proving nothing.

### Multi-package entity graphs

Entities in several packages were already scanned together, reconciled together
and generated per package, and that is now stated, tested and documented rather
than left to be discovered. The boundary is one relation: a relation may not
cross a package boundary, because its loader is generated into the declaring
entity's package and needs the target's descriptors, which are unexported in the
target's. The reverse direction is not representable at all — two entity
packages naming each other's types are an import cycle, which Go rejects before
this tool sees them.

- **`E024` (new): relation target is in another package.** Adding a code is an
  additive API change, and it is made now rather than after v1 for that reason.
  Previously the refusal came only from the code emitter, which runs *after*
  `orm makemigrations` and `orm migrate` — so the tool accepted the model,
  wrote a migration for the foreign key, applied it, and only then said the
  relation could not be generated. E024 is reported by reconciliation, so
  `orm check` says it first. The foreign key itself is legitimate and is still
  planned and applied; what is refused is only the generated loader.

- Two error messages stopped describing the project's own development. "this
  milestone does not generate across" and "this milestone writes into the
  entity's own package" said nothing useful to a reader who does not know what a
  milestone is.

- `packages:` with more than one entry, and the `output:` field, are documented
  in the README along with the workaround for a relation you cannot have: cross
  the boundary with the foreign key column instead.

### Fixed

- **`orm makemigrations` could write a migration that drops columns because of a
  mistake in Go.** Two entities describing the same table each contributed a
  table to the desired schema, and the differ compared the database against
  whichever of the two it reached — so a second, partial view of a table was
  read as an instruction to drop every column it did not mention. The command
  exited zero and printed only the ordinary data-loss warnings, which describe
  the operation and not its cause. Reconciliation had always caught this as
  E017, but reconciliation compares Go against the database and does not run on
  that path: in managed mode the declarations *are* the schema. `desired.Build`
  now refuses, naming both entities and the table. Two packages make the mistake
  easy to reach — neither can see the other's `//orm:table` — but a single
  package hit it identically.

- **Raw queries were invisible to tracing.** `orm.Raw(...).All/One/Rows` emitted
  no trace event at all, so `observe.StartEvent.Raw` was never set by anything
  and both `ormslog.WithRawSQL` and `ormotel.WithRawSQL` — the switches that
  exist because caller-written SQL may contain literals the ORM cannot redact —
  could never fire. The raw path is now instrumented like every other terminal,
  with `Raw: true` on the event; the stream's span ends when the iteration does,
  however it ends. The test that should have caught this printed the event count
  and asserted nothing; it now asserts it.

- **`ormhealth.WithMigrationState` reported every migration as pending on a
  fully migrated database.** The history query named a column the migration
  engine does not write (`id` rather than `migration_id`), so it always failed —
  and the failure path treated any error as "this database has never been
  migrated", which is both the most alarming answer available and, here, wrong.
  A failure that is not the history table being absent is now reported as
  `StatusUnknown` with the cause attached. Found by the production example,
  which was the first thing to ask a real deployment's migration state.


### Performance intelligence (M14)

Ways to see what a query is and what PostgreSQL did with it. No way to change
either: PostgreSQL plans, the ORM observes and reports.

**M14 intentionally contains no index advisor.** No `Advisor`, no
`RecommendIndex`, no `Optimize`, no `AutoTune` — and no advice inside a
diagnostic either. Two permanent tests run every finding the package can emit
through the phrases a recommendation would need. Deciding whether an index is
worth adding needs a whole workload; a tool looking at one statement has none of
it, and a confident-looking wrong recommendation gets followed.

#### Typed EXPLAIN (M14.1)

- `Explain` plans without executing; `ExplainAnalyze` executes. The names differ
  because the behaviours differ dangerously, and permanent tests assert row
  counts either side of both.
- `FORMAT JSON` internally, always. The text format is never parsed.
- Typed options with a central PostgreSQL version-capability model, tested on 14,
  16 and 17.

#### Plan model (M14.1)

- `plan.Plan` and `plan.Node`, with optional fields as pointers so an absent
  field is distinguishable from a reported zero.
- `NodeType` is a string: a future PostgreSQL node parses rather than failing.
  Unknown JSON fields are preserved rather than rejected.
- Traversal helpers, stable node IDs and tree paths, and a derived `Summary`
  kept clearly separate from the server's own fields.

#### Fingerprints (M14.2)

- Versioned (`v1:…`), value-free and deterministic. Structure changes it;
  bind values do not.

#### Tracing (M14.3)

- An interface and event types, with no telemetry dependency.
- **Bind values never reach an event.** The exception is documented: SQL written
  by the caller inside `Raw` may contain literals, and the ORM does not parse SQL.

#### Diagnostics (M14.4, M14.5)

- Static findings (`QS…`) read the query's structure through a narrow internal
  boundary that hands over names and counts, never the tree — so a `Shape` is
  safe to log. `Raw` reports that structural analysis is unavailable rather than
  inventing sources from a string.
- Plan findings (`PL…`) read what PostgreSQL reported: rows removed, spilled
  sorts, hash batches, temp I/O, loop counts, index rechecks, workers, buffers,
  WAL, non-default settings, and misestimates compared per loop.
- Severity and confidence are separate fields, because how much a finding matters
  and how sure it is come apart constantly.
- The relation-loading statement count is exact and stated as a number, because
  the loader batches — which is the opposite of warning about N+1.
- No finding judges a scan type. `PL003` was retired rather than reused.

#### Performance report (M14.6)

- `PerformanceReport` gathers everything and **never executes the statement**;
  `PerformanceReportAnalyze` does and says so; `ReportFromPlan` uses a plan you
  already have.
- Sections separated by provenance: ORM structure, PostgreSQL estimate,
  PostgreSQL measurement, derived.
- A plan carries the bind values PostgreSQL wrote into its conditions. The raw
  plan keeps them, no finding quotes them, and the rendering withholds them —
  `Render(WithConditions())` opts back in.

#### Compiled queries (M14.7)

- Investigated and deferred on benchmark evidence. pgx's server-side statement
  cache is a different thing, and M14 does not add a second one.

### PostgreSQL power (M12)

Network types, JSONB, full text search and row locking, each as PostgreSQL's own
rather than as a generic feature translated into it.

#### PostgreSQL-native types (M12.2)

##### Ranges and multiranges

- `orm.Range[T]` and `orm.Multirange[T]` are the range model, generic over the
  element type. Six range families and six multirange families map end to end:
  `int4range` → `Range[int32]`, `int8range` → `Range[int64]`, `numrange` →
  `Range[T]` for the configured numeric type, and `daterange`, `tsrange` and
  `tstzrange` → `Range[time.Time]`, plus the multirange of each.
- The three that share `time.Time` stay three different PostgreSQL types. The
  catalog decides in database-first mode; in managed mode a `Range[time.Time]`
  becomes `tstzrange` unless a `pgtype:` tag names `daterange` or `tsrange`,
  which is the same rule a bare `time.Time` field already follows.
- `Range[T]` carries PostgreSQL's whole model — inclusive, exclusive and
  unbounded on each side, and empty as a state of the value — because a struct
  of two endpoints cannot say which of those it is. Constructors: `Closed`,
  `Open`, `ClosedOpen`, `OpenClosed`, `RangeFrom`, `RangeUntil`, `EmptyRange`,
  `UnboundedRange`, and `NewRange` for the general form.
- SQL NULL is not a state inside a range. A `NOT NULL` column is `Range[T]` and
  a nullable one is `*Range[T]`, as everywhere else.
- PostgreSQL canonicalises: a discrete range comes back rewritten (`[1,10]`
  becomes `[1,11)`), and a multirange comes back sorted, merged and with its
  empty components dropped. A round trip returns the value the server holds, and
  nothing here pretends otherwise.
- `RangeCol[E,T]`, `NullRangeCol[E,T]`, `MultirangeCol[E,T]` and
  `NullMultirangeCol[E,T]` carry the nine range operators — `Contains`,
  `ContainsRange`, `ContainedBy`, `Overlaps`, `StrictlyLeftOf`,
  `StrictlyRightOf`, `NotRightOf`, `NotLeftOf`, `Adjacent` — and the six
  functions `Lower`, `Upper`, `IsEmpty`, `LowerInc`, `UpperInc`, `LowerInf`,
  `UpperInf`. The `Range*` and `Multirange*` package functions are the same
  operators over arbitrary expressions.
- `lower()` and `upper()` are nullable even over a `NOT NULL` column, because
  an empty range and an unbounded one have no bound to return. The descriptors
  type them that way, so the case that would otherwise scan a NULL into an
  `int32` is a compile error instead.
- GiST indexes over range columns round-trip through the managed workflow with
  zero drift; nothing about them is range-aware.

##### Intervals

- `interval` maps to `orm.Interval`, which keeps `Months`, `Days` and
  `Microseconds` apart. It is deliberately not `time.Duration`: a duration has
  nowhere to put "one month", and writing one as 30 days gives the wrong answer
  for most of the dates it is added to.
- `Interval.Duration` converts only when months and days are both zero, and
  returns `ErrCalendarInterval` otherwise rather than approximating.
  `IntervalFromDuration` goes the other way exactly, truncating to PostgreSQL's
  microsecond resolution.
- `AddInterval`, `SubInterval`, `IntervalPlus`, `IntervalMinus`,
  `IntervalTimes` and their nullable forms, typed with PostgreSQL's own
  results — including `date + interval` being a `timestamp` rather than a date.

##### Known limitation

- A Go type alias is not recognised. Since Go 1.23 an alias is its own
  `go/types` node, so `type Period = orm.Range[time.Time]` reaches the scanner
  carrying neither a qualified name nor type arguments and will not be mapped
  as a range. Write the instantiation, or declare a defined type over it.

##### Network types

- `inet` and `cidr` map to `netip.Prefix` and `macaddr` to `net.HardwareAddr`,
  end to end: introspection, reconciliation, managed schema, migrations,
  generated descriptors, projection, writes and COPY. pgx encodes all three
  natively, so nothing needs registering.
- `orm.ContainedBy`, `ContainedByOrEquals`, `ContainsNetwork`,
  `ContainsNetworkOrEquals`, `NetworksOverlap` for `<<`, `<<=`, `>>`, `>>=`
  and `&&`; `Host`, `MaskLen`, `Network` and their nullable forms.

#### JSON and JSONB (M12.3)

- `JSONGet`/`JSONIndex` (`->`), `JSONText`/`JSONIndexText` (`->>`),
  `JSONPathGet` (`#>`), `JSONPathText` (`#>>`) — paths travel as `text[]` bind
  parameters, so a key containing a quote is a key.
- `JSONContains`/`JSONContainedBy` (`@>`, `<@`), `JSONHasKey`/`JSONHasAnyKeys`/
  `JSONHasAllKeys` (`?`, `?|`, `?&`).
- `JSONSet`/`JSONInsert`/`JSONStripNulls` and their nullable forms, usable in a
  select list, an `UPDATE ... SET` and a `RETURNING` clause.
- `JSONArrayLength`, `JSONTypeOf`, and the JSONPath predicates `JSONMatches`
  (`@@`) and `JSONPathExists` (`@?`).
- SQL NULL, JSON null and a missing key stay three different answers, checked
  against PostgreSQL for all five document shapes.

#### Full text search (M12.4)

- `tsvector` and `tsquery` map to the named types `orm.TSVector` and
  `orm.TSQuery`, end to end — so a parsed document is not an ordinary string.
- `ToTSVector`, `ToTSQuery`, `PlainToTSQuery`, `PhraseToTSQuery`,
  `WebSearchToTSQuery`, all taking a `TextSearchConfig` bound as a regconfig
  value rather than spliced in as text.
- `Matches` (`@@`), `TSRank` and `TSRankCD` (both `real`, as PostgreSQL returns
  them), `SetWeight`, `Concat2TSVector`, and the query combinators
  `AndTSQuery`, `OrTSQuery`, `FollowedByTSQuery`, `NotTSQuery`.

#### Row locking (M12.5)

- `Query.Lock`, `SelectQuery.Lock` and `ComposedQuery.Lock` take a strength —
  `ForKeyShare`, `ForShare`, `ForNoKeyUpdate`, `ForUpdateStrong` — and options
  `NoWait`, `SkipLocked` and `LockOf`. `ForUpdate()` is unchanged.
- Two waiting policies at once are refused; a lock target the statement does not
  select from is refused; a derived table or CTE is refused, because rows it
  produces are not rows of a table.

#### Fixed during the M12 audit

- **`TSRank` and `TSRankCD` claimed a non-nullable score for a nullable
  vector.** Both accepted a vector that may be NULL — a nullable column, or one
  read through an outer join — and typed the result `float32`. PostgreSQL
  returns NULL, so the query failed with a scan error against a type that said
  it could not. They now take the non-nullable forms, and `TSRankNull` /
  `TSRankCDNull` take the nullable ones and return `*float32`.
- **`JSONIndex` and `JSONIndexText` never worked.** PostgreSQL has a text-key
  and an integer-subscript form of `->` and `->>`, and a bare parameter carries
  no type — so the server resolved the index to text and the statement failed
  to encode. The index is now cast to `int4`, which picks the operator while
  leaving the index a value.

#### Fixed

- **The generator panicked on a type with no doc comment.** Schema declarations
  are read from a type's comment group, and the group is nil when there is
  none — which no fixture had ever reached, because every type in every one of
  them carried a comment. It is a nil check.

### Bulk ingestion with COPY (M12.1)

PostgreSQL's copy protocol, as itself rather than as a large INSERT.

- `Repo.CopyFrom` streams entities into the table over pgx's `CopyFrom`, using
  the generated value accessors — no reflection on the row path. The columns
  are the entity's writable ones; a generated or identity column is never
  mentioned, so PostgreSQL supplies it.
- `Repo.CopyFromSeq` takes an `iter.Seq2[E, error]` and pulls one row at a time,
  so a source larger than memory is a source this can ingest. An error the
  sequence yields stops the COPY and is returned wrapped.
- `orm.CopyColumns` and `orm.CopyColumnsFromSeq` copy a chosen subset. Every
  column left out takes the table's own DEFAULT, identity or NULL — nothing
  substitutes a Go zero for a column nobody named, and a zero that was named is
  a value like any other.
- `orm.CopyExecutor` is the pgx capability a COPY needs; `*pgxpool.Pool`,
  `*pgx.Conn` and `pgx.Tx` satisfy it as they are, so a repository bound to a
  transaction copies inside that transaction. An executor that cannot COPY is
  told so rather than silently falling back to an INSERT.
- What COPY does not have is absent rather than approximated: no RETURNING, no
  ON CONFLICT, and no invented per-row index — PostgreSQL reports a failure
  against the stream, and the error is passed through with the table added and
  nothing else.

Benchmarked against `InsertMany` on the same table and connection: at 10,000
rows COPY is roughly 1.8x faster and allocates about 14x less. `InsertMany`
remains the call when the rows have to come back.

### Advanced SQL composition (M11)

A typed query becomes a typed SQL source, and sources compose. Derived tables,
subqueries, joins, CTEs and window functions are one feature rather than five,
because all of them nest the same tree through the same compiler.

- **Composed queries.** `orm.Compose(ex, projection)` builds a SELECT whose FROM
  clause is a list rather than a table, with `From`, the joins, `Where`,
  `GroupBy`, `Having`, `OrderBy`, `Limit`, `Offset`, `Distinct`, `DistinctOn`,
  `With`, `ForUpdate`, `Clone`, `SQL`, `All`, `One`, `Rows`, `Count`. The result
  shape is an ordinary `ProjectN` instantiated at `orm.Composed`, so the
  scanning is M10's — typed locals, one `Scan`, no reflection. `orm.Rows`
  builds the other kind: a query meant to be a source, whose declared outputs
  are its select list and its column names at once.
- **Lifting.** `orm.Of`, `orm.OfNull`, `orm.Opt` and `orm.Cond` carry an
  entity's expressions and predicates into a composed query. Checking moves from
  the entity tag to source identity, which is structural, sequential and never
  disabled. `orm.Eq`, `Ne`, `Lt`, `Lte`, `Gt`, `Gte` compare across sources by
  value type, so a nullable foreign key compares to the key it points at.
- **Derived tables and CTEs.** `orm.Sub`, `orm.CTE`, `orm.Named`/`orm.NamedNull`
  and `orm.Ref`/`orm.OptRef`. Columns are typed by the declaration they were
  built from rather than addressed by a string, and a column a source does not
  provide is refused with the columns it does have. `orm.RecursiveCTE` and
  `orm.RecursiveCTEUnion` for `WITH RECURSIVE`, whose recursive term is checked
  against the anchor's arity, names and nullability. `orm.WritingCTE` makes an
  `INSERT`/`UPDATE`/`DELETE ... RETURNING` a WITH item. `orm.Materialized` and
  `orm.NotMaterialized` are declaration options.
- **Public joins.** `Join`, `LeftJoin`, `RightJoin`, `FullJoin`, `CrossJoin` and
  the `LATERAL` forms, over any source. A join condition may name the sources
  introduced before it and the one it attaches — naming one joined later is
  refused, with a message about the order. A non-lateral derived table
  correlating to a sibling is refused too.
- **Source-induced nullability.** A value read through an outer join is nullable
  in that query whatever its column's constraint says. `orm.Opt` and
  `orm.OptRef` widen the result type; a select list that does not is refused
  rather than left to fail as a scan error. Aggregates and a `COALESCE` with a
  non-nullable fallback are barriers, so `count(p.id)` and
  `coalesce(s.count, 0)` stay non-nullable. Nothing about the generated
  descriptors changes.
- **Subquery expressions.** `orm.Exists`, `orm.NotExists`, `orm.InSub`,
  `orm.NotInSub` and `orm.Scalar`. A scalar subquery is always nullable, because
  a statement matching no row yields NULL whatever it selects. `NOT IN` keeps
  PostgreSQL's three-valued logic and is never rewritten into `NOT EXISTS`.
- **Expressions.** `Case`/`When`/`Else`/`End`, `Coalesce`, `CoalesceNull`,
  `NullIf`, `Cast`, `CastNull` with a type registry (`PGType`, `PGTypeOf`),
  `Lower`, `Upper`, `Trim`, `Length`, `Concat`, `Now`, `DateTrunc`, `Extract`,
  `Row2Eq`, `Row2In`, the array operators and the JSON operators whose result
  type and nullability can be stated exactly. A `CASE` with no `ELSE` is
  nullable; `NULLIF` always is.
- **Window functions.** `RowNumber`, `Rank`, `DenseRank`, `PercentRank`,
  `CumeDist`, `Ntile`, `Lag`, `LagN`, `Lead`, `LeadN`, `FirstValue`,
  `LastValue`, `NthValue`, and `.Over(...)` on any aggregate. `orm.Window()`
  with `PartitionBy`, its own `OrderBy` and `ROWS`/`RANGE`/`GROUPS` frames.
  `Lag` and `Lead` are nullable whatever they read. A window function is refused
  in `WHERE`, `HAVING`, `GROUP BY`, `DISTINCT ON` and a join condition, which is
  where PostgreSQL refuses it — filter on one through a derived table.
- One statement, one parameter list. A CTE, a derived table, a join condition, a
  correlated subquery and a window expression all allocate their placeholders
  from the same writer, in the order the SQL is written.
- `docs/composition.md` documents all of it, including exactly what the Go
  compiler refuses, what the builder refuses, and what is left to PostgreSQL.

`Query.With` still loads relations and `ComposedQuery.Join` still composes SQL;
neither is implemented in terms of the other.

#### Fixed during the M11 audit

- **A recursive CTE could be typed non-nullable while returning NULL.** The
  convergence check reads whether a term's expression can be NULL, and the
  typed wrappers were not recording it: a nullable column passed straight to
  `Named`, or a nullable aggregate such as `Min`, looked non-nullable. A
  recursive term selecting one under a NOT NULL anchor was accepted, and the
  query failed with a scan error against a column the types said could not be
  NULL. Every descriptor, aggregate and scalar subquery now states its own
  nullability.
- **`orm.Nullable` was refused through an outer join.** The widened value was
  not recorded as nullable, so a legal query was rejected. Same fix.
- **One source could be introduced twice in one FROM clause.** `FROM users JOIN
  users` names one table twice and makes every column of it ambiguous;
  PostgreSQL refuses it and the builder now does too, naming the occurrence. A
  second occurrence needs its own alias, which `As` returns.
- **`LATERAL` before a table or a CTE reference** reached PostgreSQL as "syntax
  error at end of input". It is refused by the builder, which knows the source
  is not a subquery.

### Typed projections, aggregates and richer writes (M10)

A second query model beside the entity one. An entity query returns entities; a
projection query returns the expressions you selected, in a Go shape bound at
compile time.

- `orm.Select(repo, projection)` with `Where`, `OrderBy`, `Limit`, `Offset`,
  `Distinct`, `ForUpdate`, `Clone`, `SQL`, `All`, `One`, `Rows`, `Count`,
  `Exists`. `ProjectN` binds N expressions to a function of exactly their N
  result types, so a column of the wrong type does not compile — and neither
  does a nullable column bound to a destination that cannot hold NULL.
- Scanning uses no reflection and allocates its destinations once per query
  rather than once per row.
- Aggregates typed by PostgreSQL rather than by Go: `sum(integer)` is `bigint`,
  `sum(bigint)` and every `avg` of an integer are `numeric` read through the
  configured mapping, and every aggregate but `count` returns a pointer because
  an aggregate over no rows is NULL. `GroupBy`, `Having`, `FILTER` and
  `count(DISTINCT x)`.
- Expression assignments: `Users.LoginCount.SetExpr(Users.LoginCount.Add(1))`.
  Literals inside them are parameters.
- `UpdateReturning`, `UpdateReturningEntity`, `DeleteReturning`,
  `DeleteReturningEntity` — values from the write itself, never a second
  statement. `One` runs the whole write and reports `ErrMultipleRows`
  afterwards rather than adding a `LIMIT` that would change what was modified.
- `ON CONFLICT ... DoUpdateSet` with typed `EXCLUDED` and a conflict `WHERE`.
  `EXCLUDED` is a node, not a string, and using it outside a conflict clause is
  refused before the statement runs.

#### Fixed during the M10 audit

- **Nullability now travels with the expression, not only with the column.**
  A nullable column's `Value()` and its arithmetic were inherited from the
  non-nullable base and claimed a result that could never be NULL —
  `Users.Bio.Value()` was `Value[E, string]`, and `Users.Visits.Add(1)` was
  `Value[E, int64]` although `NULL + 1` is `NULL`. Each nullable descriptor now
  shadows those, `AddCol`/`SubCol` take a non-nullable operand, and `Excluded`
  infers the nullable form for a nullable column. Six compile-fail cases hold
  the line.
- `ExcludedNull` is removed. `Excluded` now infers the nullable form itself, so
  the second function was a duplicate spelling of one thing.

Nothing in M0–M9 changed. `db.Users.Query()` and `Query.Count` mean exactly what
they did.

### Managed schema mode and migrations

A second way to use the tool, alongside the database-first one, which is
unchanged. `schema.mode: managed` makes the Go declarations the source of truth
for the schema and adds a migration workflow that carries them to PostgreSQL.
Nothing about the runtime changed.

- Schema declarations on entities: `pk`, `identity`, `unique`, `default:`,
  `generated:` and `pgtype:` field tags, and `//orm:index`, `//orm:unique`,
  `//orm:check`, `//orm:enum` and `//orm:extension` on the struct. Declarations
  name Go fields, so a project can describe its schema before anything has been
  generated into it.
- `orm makemigrations` computes a migration from the declarations and the
  migrations already committed, never from a live database. `--check` for CI,
  `--dry-run`, `--sql`, `--name`, and `--baseline` for an existing database.
- Rename detection asks rather than guesses, and refuses to guess in CI:
  `--rename`, `--rename-table`, `--no-rename`, `--no-input`.
- `orm migrate`, with `--plan`, a target migration, and `--fake` — which
  verifies that the database really is in the state it is about to record.
- `orm showmigrations`, `orm sqlmigrate` (with `--reverse`) and `orm inspect`
  (with `--json`).
- Migration artifacts are deterministic JSON files under `migrations/`, checksummed
  over their operations rather than their bytes. `raw_sql` and `state_only`
  operations are hand-editable.
- Atomic migrations run in one transaction together with the row recording them.
  An operation PostgreSQL refuses inside a transaction block — `CREATE INDEX
  CONCURRENTLY` — makes the migration non-atomic, which every command says out
  loud.
- `orm check` in managed mode reports the model, migration, database, mapping and
  generated states separately, and distinguishes "write a migration" from "run a
  migration" from "somebody changed the database". Advisory warnings carry stable
  codes W201–W211.
- `orm generate` in managed mode refuses when the three schema states disagree.
- `orm init --mode managed` writes a starting configuration.
- New: [`docs/migrations.md`](docs/migrations.md) and
  [`examples/managed`](examples/managed).

### Fixed

- **A unique index is one object, in one place.** The desired schema recorded a
  declared unique index both as an index and as a uniqueness object, and
  introspection recorded it only as the latter. Two consequences, both bad: the
  `CREATE TABLE` for a project declaring `//orm:index ... unique` built the index
  twice and the second statement failed on a name the first had taken, so the
  first migration of such a project could not run; and the two sides disagreed
  about where the object lived, so every diff proposed to move it. A unique index
  now lives in `Table.Indexes` with `Unique` set, on both sides. `Table.Uniques`
  holds UNIQUE constraints.
- **An index over an expression is read back as one.** PostgreSQL renders a
  function-call key without the parentheses it puts round other expressions, so
  `lower(title)` arrived looking exactly like a column name and was recorded as
  one. The table now decides: a key naming no column of it is an expression.
- **A migration file can no longer panic the tool.** An artifact naming an
  identifier PostgreSQL cannot quote — empty, or carrying a NUL — reached the
  renderer, whose guard against generator bugs is a panic. Such an artifact is
  now refused when it is read, with the operation named.
- **A migration ID is a name, not a location.** `Store.Write` accepted IDs
  containing a path separator or a leading dot, which wrote the file outside the
  migrations directory or somewhere the loader does not look — a migration
  written and then silently absent from its own history.
- An operation the diff produces to carry a refusal — removing an enum label —
  no longer applies as a no-op, which would have let a reconstructed state
  silently disagree with what the migration claimed.
- `DropIndex` is classified as locking rather than safe, matching `CreateIndex`:
  both take the same lock, and `CONCURRENTLY` is what avoids it.
- The migration diff now compares schema expressions the way the drift check
  does, normalising the casts and parentheses PostgreSQL adds when it stores and
  re-renders one. Without it, a database adopted as a baseline immediately
  wanted a migration to change a default into the identical default.
- Column differences are reported by name rather than by position, so a dropped
  column reads as one missing column rather than as several that disagree.
- `orm migrate --plan`, `orm showmigrations` and `orm check` no longer create the
  migration history table; they are read-only.
- Widening a column no longer makes every check and default mentioning it look
  changed for ever: PostgreSQL re-renders those as `(col)::type`, and the
  parentheses left behind when the cast was stripped compared as a difference.
- `orm inspect` reports the extensions the database has, which it never read.

## v0.1.0

The first release. It contains a complete reconciliation, read, write and
relation-loading stack for PostgreSQL.

### Schema reconciliation

- `orm check` reads your entity structs and the real database catalog and
  reports every place they disagree, with stable finding codes (E001–E023,
  W001–W00n), source positions and a suggested fix.
- Text, JSON and GitHub-annotation output. All three are deterministic.
- Nullability, defaults, identity and generated columns, enums, domains,
  arrays and configured type overrides are all proved rather than assumed.
- Relations are resolved from `pg_constraint` and `pg_index`: which table
  carries the foreign key, and whether a uniqueness proof makes it a has-one.

### Generation

- `orm generate` emits typed column descriptors, entity metadata, reflection-free
  scanners, relation loaders and a `DB` handle, and refuses to write anything
  from a mapping that does not hold.
- Output is deterministic and atomic: every file is rendered and formatted before
  any is written, and each lands through a temporary file and a rename.
- An `orm.lock` records a canonical fingerprint of the mapping. `orm check
  --generated` reports when committed generated code no longer matches the
  schema — a change nothing else in the toolchain notices.
- `RegisterTypes` is generated for the PostgreSQL types pgx has to be taught.

### Queries

- Typed dynamic predicates: which comparisons a column has is decided by its
  PostgreSQL type, so `IsNull` on a `NOT NULL` column does not compile.
- `Where`, `OrderBy`, `Limit`, `Offset`, `ForUpdate`, `Clone`, table aliases.
- `All`, `One`, `Count`, `Exists`, `Rows` — the last a genuine stream.
- `orm.Expr` for a SQL fragment, with its placeholders renumbered into the
  surrounding statement and its arguments still bound.
- Scope validation: a predicate over a table the query does not select from is
  reported before the statement is sent.
- `SQL()` renders the statement and its arguments separately. Values are never
  in the text.

### Writes

- `Insert`, `InsertMany`, `Update`, `Delete`, `OnConflict` with `DoNothing` and
  `DoUpdate`.
- A Go zero value is a value: `Active: false` stores `false`. `orm.Default` is
  how you ask for the column's default instead.
- An update or delete with no `WHERE` is refused unless `All()` says otherwise.

### Relations

- `With` loads what you ask for and nothing else. There is no lazy loading.
- A plain to-one folds into the root statement; a to-many is batched with
  `WITH ORDINALITY`, so PostgreSQL decides which rows relate and Go attaches by
  ordinal — correct for `citext`, `numeric`, domains and composite keys.
- Relation options: `Where`, `OrderBy` and a per-parent `Limit` implemented with
  `LATERAL`.
- `Any` and `None` filter root rows with correlated `EXISTS` and load nothing.
- Nested `With` to any depth, loaded breadth-first. The number of statements
  follows the shape of the requested tree and never the number of rows in it.
- Relations whose foreign key has no Go field are loaded from the key the
  parent's statement selects, so the ORM never dictates a struct's shape.

### Runtime

- `db.Tx` and `db.TxOptions`, with savepoint-backed nesting. Rolls back on
  error, re-panics after rolling back on panic, retries nothing.
- `orm.Raw` runs a statement you wrote and keeps the generated scanner.
- PostgreSQL errors remain reachable with `errors.As` through every layer of
  context.
- Everything runs through the pgx executor you supply, so pgx tracing and
  metrics observe all of it.

### Tooling

- `orm init`, `orm check`, `orm generate`, `orm explain`, `orm version`.

### Known limitations

PostgreSQL only. No migrations. No public arbitrary `JOIN`, projections,
aggregates beyond `Count`, `GROUP BY`, CTEs, window functions or set operations.
No identity map, dirty tracking, `Save`, cascading writes or lifecycle hooks.
Relations must live in one Go package. Relation key chunking is not implemented:
keys travel as one array parameter per column, which has no practical ceiling.
