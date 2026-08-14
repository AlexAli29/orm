# Performance intelligence

M14 adds ways to see what a query is and what PostgreSQL did with it. It adds
no way to change either.

That division is the whole design:

> PostgreSQL plans. The ORM observes, structures and reports.

## M14 intentionally contains no index advisor

There is no `Advisor`, no `RecommendIndex`, no `SuggestedIndex`, no `Optimize`
and no `AutoTune` — not as an API, not as a hidden heuristic, and not as a
sentence inside a diagnostic. Two permanent tests run every finding the
diagnostics package can produce through a list of phrases a recommendation
would need, so the line cannot drift back in.

The reason is not modesty. Deciding whether an index is worth adding needs the
whole workload, not one statement: the write cost it adds, the other queries it
helps or does not, how big the table will be next year, what the maintenance
window is, and who is accountable for the server. A tool looking at a single
query has none of that. A confident-looking wrong recommendation is worse than
no recommendation, because it gets followed.

So M14 reports facts and stops:

```
Node:                    Seq Scan on posts
Estimated rows:          24
Actual rows:             21
Rows removed by filter:  184,291
Sort:                    external merge, 38 MB on disk
```

What to do about that is yours, your DBA's, or a higher-level tool's. The
structured output exists so that such a tool can read the facts without
scraping terminal text.

### Why a sequential scan is not a finding

A sequential scan is very often the fastest plan PostgreSQL has: for a small
relation, or a predicate that matches most rows, reading the table beats
bouncing through an index and back to the heap. `SEQ_SCAN_BAD` would be wrong
more often than right.

What M14 reports instead is the measurement — how many rows a node read and how
many it kept — under `PL002`. That is a fact whatever the plan; reading it is
how you decide whether the scan was a problem.

## EXPLAIN and EXPLAIN ANALYZE

```go
p, err := q.Explain(ctx)         // plans the statement; does not run it
p, err := q.ExplainAnalyze(ctx)  // RUNS the statement, and measures it
```

The names differ because the behaviours differ dangerously. `Explain` asks
PostgreSQL to plan the statement and throw the plan away: an `UPDATE` explained
this way updates nothing. `ExplainAnalyze` executes: an `UPDATE` analysed this
way performs the write, a `DELETE` deletes, and a statement calling a volatile
function does whatever that function does.

Both use `FORMAT JSON` internally. The human text format is never parsed.

### Rolling back an analysed write

```go
tx, _ := db.Begin(ctx)
report, err := orm.PerformanceReportAnalyze(ctx, tx, stmt, shape)
tx.Rollback(ctx)
```

This undoes **PostgreSQL's own transactional effects**. It is not a sandbox: a
function that sent an email, wrote to a file or called out to another service
has already done those things, and no rollback reaches them. The ORM never
creates this transaction for you, because pretending the pattern is universal
safety is how it gets used where it is not.

## The plan

`plan.Plan` is PostgreSQL's plan, typed. Optional fields are pointers so that a
field the server did not report is distinguishable from one it reported as
zero — a node that scanned no rows and a node that never ran are different
facts. Node types are strings, not an enumeration, so a future PostgreSQL node
parses rather than failing.

Estimates and measurements are different fields. `PlanRows` is what the planner
expected; `ActualRows` is what the executor counted, **per loop** — a node
inside a nested loop that ran a thousand times reports the average of one
execution. `Plan.Analyzed` says which kind of plan you have.

## Fingerprints

A fingerprint identifies a statement's *shape*. Two executions differing only in
their bind values share one; changing an operator, a join type, an ordering
direction, a lock mode or a projected column does not.

It is versioned (`v1:…`). The contract is: the same ORM fingerprint version and
the same query structure produce the same fingerprint across processes and runs.
It is deliberately not promised across future major algorithm changes, which is
what the version is for.

It is not a cache key, not a security token, and not a replacement for
PostgreSQL's `queryid`.

## Tracing

The core exposes an interface and event types and imports no telemetry library.
Adapters belong outside it.

Explaining is itself a traced operation: `Explain` emits `observe.OpExplain` and
`ExplainAnalyze` emits `observe.OpExplainAnalyze`, carrying the statement's own
SQL with its placeholders — never the plan, which is the server's output and
carries the constants it planned with.

**Bind values never reach a trace event.** The SQL in an event keeps its
placeholders — `WHERE email = $1` — which is useful and safe. A permanent test
runs passwords, tokens, emails, JSON documents, geometry and byte slices through
every statement kind and searches every event for them.

The exception is honest and unavoidable: SQL you wrote yourself inside `Raw` may
contain literals, and the ORM cannot redact those without parsing SQL, which it
does not do.

That is why a raw statement is traced as raw. `observe.StartEvent.Raw` marks it,
and both adapters keep the SQL of a raw statement behind a switch of its own —
`ormslog.WithRawSQL`, `ormotel.WithRawSQL`, off by default — separate from the
switch that logs generated SQL. Turning on statement logging therefore does not
turn on the logging of your literals; that takes a second, deliberate decision.

## Diagnostics

Two kinds, kept apart:

- **Static** (`QS…`) reads the query's own structure. It needs no database and
  is available in a unit test. For `Raw` it reports that structural analysis is
  unavailable rather than inventing sources out of a string.
- **Plan** (`PL…`) reads what PostgreSQL reported, and is only as good as the
  plan it was given.

Severity is how much a finding matters; confidence is how sure it is. They come
apart constantly, which is why they are separate fields. Nothing here is an
error: a slow query is still a correct one.

### The N+1 question has a number, not a warning

The relation loader batches, so the statement count is exact before anything
runs: one for the root, one per batched relation step, whatever the number of
rows. A to-one relation that folds into the join costs none. So the report
states the count and says it does not grow — which is the opposite of warning
about N+1 because relations are in use.

## The performance report

```go
report, err := q.PerformanceReport(ctx)
```

Gathers the fingerprint, the statement's structure, PostgreSQL's plan and the
findings from both. **It uses plain EXPLAIN and never executes the statement.**
For measurements, `PerformanceReportAnalyze` — whose name says so — or hand a
plan you obtained deliberately to `ReportFromPlan`.

Sections are separated by provenance: what the ORM knows structurally, what
PostgreSQL estimated, what PostgreSQL measured, what was derived. The rendering
is built from the fields; anything reading a report in a program should read the
fields.

### The plan carries values, and the rendering does not

PostgreSQL plans a parameterised statement with the values it was given and
writes them into the conditions it reports:

```
Index Cond: (email = 'someone@example.com'::text)
```

That is the server's output. Removing the literal would mean parsing SQL.

So `Report.Plan` keeps PostgreSQL's answer verbatim, no diagnostic quotes a
condition string, and `Report.String` — the thing that ends up in a log — leaves
the condition text out, printing `(withheld)` in its place. `Render(WithConditions())`
brings it back for someone reading a plan by hand who knows what is in it.

**Encoding a report is safe too.** `json.Marshal(report)` is the obvious thing
to do with a structured report and is exactly what feeds a log pipeline, so it
encodes `RedactedPlan` — every node type, relation, index, cost, row count, loop
count, buffer and timing, with the free-form expression text removed — and omits
the raw plan. `Report.Plan` is still there as a field for a caller who wants the
server's verbatim answer; encoding *that* is then a deliberate act
(`json.Marshal(report.Plan)`) rather than something that happens by accident.

`plan.Plan.Redacted` clears the conditions (`Filter`, `Index Cond`,
`Recheck Cond`, `Join Filter`, `Hash Cond`, `Merge Cond`, `TID Cond`,
`One-Time Filter`) together with `Output`, `Sort Key`, `Presorted Key`,
`Group Key` and the unmodelled `Extra` fields — every place an expression, and
therefore a literal, can appear.

### What is still not value-free

Four things, stated so the boundary has no soft edge:

- **`Report.Plan`** — PostgreSQL's own answer, kept verbatim on purpose.
- **`Render(WithConditions())`** — an explicit opt-in.
- **Raw SQL literals** — you wrote them; redacting them would need a SQL parser.
- **`*pgconn.PgError`** — the server writes contextual detail into its own
  errors. The ORM does not add bound arguments to telemetry; it also does not
  rewrite PostgreSQL's errors.

## Compiled queries

Investigated and deferred. See the CHANGELOG entry for M14.7 and the benchmarks:
the ORM's compile step is small next to the round trip, and pgx already caches
prepared statements server-side — which is a different thing from an ORM
compiled query, and M14 does not add a second one.
