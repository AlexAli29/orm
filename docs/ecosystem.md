# Testing, observability and the ecosystem

M15 adds convenience around the frozen ORM. It adds no SQL, no query semantics
and no second abstraction for anything the core already does.

## Testing

`ormtest` needs the ORM, pgx and the standard library — nothing else.

### A transaction that always rolls back

```go
func TestSomething(t *testing.T) {
    db := domain.New(ormtest.Tx(t, pool))

    user, err := db.Users.Insert(t.Context(), domain.User{Email: "a@b.c"})
    // assertions
}   // rolled back here
```

The rollback is registered *before* the transaction is handed back, so it
happens whatever the test does next — returning early, `t.Fatal`, or panicking.
It rolls back on a context of its own, because the test's context is already
cancelled by the time cleanups run.

What comes back is the **real** pgx transaction behind `orm.Executor`. That is
deliberate: a wrapper exposing only `Query` would break COPY inside a test
transaction and stop the ORM recognising it is in one, so the helper's
transaction would not be the thing production uses.

The consequence, stated plainly rather than glossed: a caller who type-asserts
back to `pgx.Tx` **can** commit. That is leaving this package's API, not using
it. This package offers no commit.

### Emptying tables

```go
ormtest.Truncate(ctx, ex, Users, Posts)
ormtest.TruncateWith(ctx, ex, []ormtest.TruncateOption{ormtest.Cascade()}, Users)
```

Descriptors, not strings — a typo is a compile error, and the identifiers are
quoted by the ORM's own writer through `Source.QuotedName` rather than by a
second implementation of the rules.

One statement names every table, which is what makes it work when they reference
each other. Naming only half of a reference gets PostgreSQL's error, not a
silent success; `Cascade()` is the opt-in answer and says that it empties tables
you did not name.

The executor decides the scope: a transaction rolls the truncation back with it.

### Migrations and schema drift

```go
ormtest.Migrate(t, conn, "migrations")
ormtest.RequireSchemaClean(t, "orm.yaml")
```

Both run the production path. `Migrate` is the engine the CLI runs — same
ordering, same advisory lock, same history table. `RequireSchemaClean` is the
reconciliation `orm check` runs, at the same severity threshold, printing the
same diagnostics.

That matters more than it sounds: a test helper that reimplemented either could
pass while the real thing failed, which is the one thing a test helper must
never do.

## Observability adapters

M14 defined the event model. These turn it into something a backend
understands. Both are adapters — installing one changes nothing about what a
query does or returns.

### slog

```go
db := domain.New(orm.Traced(pool, ormslog.New(logger)))
```

In the ORM's own module: `log/slog` is the standard library, so having it
available costs a project nothing.

Logs the operation, fingerprint, table, entity, relation path, argument
**count**, rows and duration. Errors are recorded by classification — SQLSTATE,
kind, constraint — not by the server's message, because PostgreSQL writes
contextual detail into those.

`WithSlowThreshold` is the caller's number. There is no default and there will
not be one: what counts as slow is a property of an application, and a number
chosen here would be wrong for most projects while looking authoritative.
Crossing it **logs**; nothing runs EXPLAIN.

### OpenTelemetry

```go
db := domain.New(orm.Traced(pool, ormotel.New(otel.Tracer("myapp/db"))))
```

A **separate Go module**. A project that never imports it never compiles
OpenTelemetry and never has it in its dependency graph — that is the whole
reason for the extra `go.mod`.

Semantic-convention keys where the conventions define one (`db.system`,
`db.query.text`, `db.collection.name`); everything else under `orm.`, a
namespace this project can define without colliding with a future convention.

The tracer is the caller's — no reaching for a global provider. Context flows
through the returned context, so an ORM span is a child of whatever span the
request already carried.

One COPY is one span, whatever the number of rows.

**It never attaches a plan.** An EXPLAIN produces a plan and a plan contains the
constants PostgreSQL planned with; the plan goes back to the caller, which is
the only place that knows whether it may be exported.

It touches neither `ConnConfig.Tracer` nor anything else pgx owns, so a project
already instrumenting pgx keeps both layers — an ORM-level span is one operation
as the application asked for it, and pgx's is the wire.

## What cannot be redacted

Unchanged from M14, and repeated here because the adapters inherit it:

- **Raw SQL literals** — you wrote them; finding them would need a SQL parser.
  `WithRawSQL(false)` withholds raw statements' SQL entirely.
- **PostgreSQL error messages** — the server writes contextual detail into its
  own errors. Both adapters record classification by default;
  `ormotel.WithErrorMessages(true)` opts into the message.
- **A raw `plan.Plan`** — PostgreSQL's own output, never attached to a span or
  a log record by these adapters.

Bind values are not on that list: the event model has no field that holds one.
