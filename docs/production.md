# Running this in production

What a service built on this ORM needs to get right, and where the working code
for each is. Everything below is exercised by tests in
[`examples/production/`](../examples/production/) and
[`examples/hexagonal/`](../examples/hexagonal/); nothing here is advice without
a test behind it.

## The shape of the wiring

```go
pool, err := pgxpool.New(ctx, dsn)     // one pool, built once
ex := orm.Traced(pool, tracer)         // tracing attaches here, and only here
svc := service.New(ex)                 // everything below takes an Executor
```

Three properties follow from that and are worth naming:

- **There is no global database.** Two of these can exist in one process against
  two databases. Nothing reaches for a package-level handle.
- **Tracing travels with the executor**, so a transaction started from it
  inherits the tracer and generated code never mentions telemetry.
- **A transaction is an executor.** Code written against `orm.Executor` takes
  part in one without knowing it is in one, which is what makes
  `orm.RunTx` composable.

## Transactions belong to the caller

```go
err := orm.RunTx(ctx, ex, func(ex orm.Executor) error {
    // every write in here is one operation
})
```

Returning nil commits; returning an error rolls back; a panic rolls back.

Data-access code should take an `orm.Executor` as its first argument and never
begin a transaction of its own. A repository cannot know whether its write is
the whole of an operation or a third of it. The layer that knows is the layer
that should say, and it says so by wrapping.

The ORM never begins a transaction you did not ask for.

## Errors: translate at the boundary, on SQLSTATE

```go
var pg *pgconn.PgError
if errors.As(err, &pg) {
    switch pg.Code {
    case "23505": return domain.ErrConflict
    case "23503": return domain.ErrInvalid
    }
}
```

Match on the five-character code, never on the message. PostgreSQL writes the
offending value into its own message — `Key (email)=(someone@example.com)
already exists` — so a translation that matched on text would be carrying one
user's data up through the application, and an HTTP handler that returned
`err.Error()` would publish it.

What a client gets is a fixed sentence per category. What the log gets is the
cause, wrapped. They are different audiences.

## Health checks are three different questions

```go
h := httpapi.NewHealth(ex,
    ormhealth.WithSchemaCheck("orm.yaml"),
    ormhealth.WithMigrationState("migrations"),
)
```

| Endpoint | Uses | Answer means |
|---|---|---|
| `/livez` | nothing | the process is alive; failing means restart it |
| `/readyz` | `ormhealth.Quick` — one statement | it can serve; failing means take it out of rotation |
| `/admin/db-health` | `ormhealth.Deep` | a report for a person, behind auth |

**Liveness must not touch the database.** A database outage is not a reason to
restart every instance of the service, and a liveness probe wired to the
database converts one outage into a restart storm at the worst possible moment.

**Readiness must have a deadline.** With the pool saturated, an unbounded probe
waits for a connection that is not coming, and the orchestrator's own timeout
ends up deciding. `TestHealth_poolExhaustion` holds every connection and checks
that the probe still answers.

The deep report is read-only — it never creates, analyses or repairs anything —
and a test snapshots the schema either side of it to keep that true.

## Shutdown has exactly one correct order

1. stop accepting connections and drain the handlers already running;
2. **then** close the pool.

```go
func (s *Server) Shutdown() error {
    defer s.pool.Close()                                // 2
    ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
    defer cancel()
    if err := s.http.Shutdown(ctx); err != nil {        // 1
        _ = s.http.Close()
        return fmt.Errorf("shutting down: %w", err)
    }
    return nil
}
```

Two details that are easy to get wrong:

- The shutdown context is **not** the application's. The application's has just
  been cancelled, and reusing it here asks the server to drain with an expired
  deadline — an immediate graceful shutdown, which is the graceful part removed.
- If the drain times out, `Close` still has to run. Otherwise the listener stays
  open while the process believes it is stopping.

`pgxpool.Close` waits for connections that are checked out, which makes the
wrong order survive the obvious test — a request already inside PostgreSQL
finishes either way. The order only becomes visible with a request that is in
flight and has **not** acquired a connection yet, which is what
`TestShutdown_inFlightRequestFinishesWithADatabase` constructs.

## Observability

The core defines an interface and two event types and imports no telemetry
library. Adapters live outside it: `ormslog` for `log/slog`, `ormotel` for
OpenTelemetry.

An executor carries **one** tracer. This does not do what it looks like:

```go
ex := orm.Traced(orm.Traced(pool, logging), tracing) // the inner one never runs
```

To reach two destinations, forward from one tracer — see
`examples/production/observability`.

**Bind values never reach an event.** The SQL keeps its placeholders. The
documented exceptions are in [`performance.md`](performance.md#what-is-still-not-value-free);
the short version is that SQL you wrote inside `orm.Raw` may contain literals
the ORM cannot redact without parsing SQL, and PostgreSQL writes its own
contextual detail into `*pgconn.PgError`.

## Migrations in a deployment

```bash
orm makemigrations --check   # fails if a declaration has no migration behind it
orm migrate --plan           # what would run
orm migrate                  # run it
orm check --generated        # the database, the declarations and the generated code agree
```

Run `makemigrations --check` in CI, not at startup. A process that migrates on
boot has three instances racing to migrate during a rolling deploy, and the
advisory lock turns that into two instances waiting rather than into a correct
plan.

`ormhealth.WithMigrationState` reports how far the running database has been
migrated, which is what tells you a deploy landed the code without the schema.

## Choosing between the two examples

Neither is the recommended one.

**`examples/production/` — pragmatic.** Four packages, no interfaces where there
is one implementation, the ORM confined to one package by convention. Fewer
files, less indirection, and the boundary holds because people keep it. Right
for most services, and right when the team is small enough that a code review
is a real check.

**`examples/hexagonal/` — ports and adapters.** The core declares what it needs,
adapters implement it, the dependency direction is enforced by a test that reads
the import graph, and the use cases can run against maps with no database at
all. Costs more files and one more indirection at every call. Right when the
core's rules are the valuable part, when more than one adapter is genuinely
coming, or when the boundary needs to survive people who did not write it.

What both do the same way, and what is not negotiable in either: no global
database, no hidden transactions, translation at the boundary on SQLSTATE, and
data-access functions that take an executor.
