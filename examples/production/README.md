# The production example

A service with users, projects and tasks, served four ways, with the
operational parts a real deployment needs.

Run the tests — they start PostgreSQL themselves through Testcontainers:

```bash
go test ./...
```

## The layers, and why there are four

```
domain/       the types and the rules. Imports no ORM, no pgx, no HTTP.
postgres/     the only package that knows this ORM exists.
service/      the use cases. Owns transactions.
transport/    net/http, chi, Gin, Fiber — the same service, four routers.
server/       what is built at startup and torn down at shutdown.
```

There are no interfaces between them. `service.Service` takes
`postgres.Store` as a struct, not as an interface it declared, because there is
one implementation and inventing a second name for it buys nothing. If a second
store ever appears, the interface goes in `service`, next to the code that calls
it. See [`hexagonal/`](../hexagonal/) for the arrangement that starts from the
interfaces instead.

## Transactions belong to the service

```go
func (s *Service) CreateProject(ctx context.Context, in domain.NewProject) (domain.Project, error) {
    return orm.RunTx(ctx, s.ex, func(ex orm.Executor) error {
        // three writes, one transaction
    })
}
```

The store's methods take an `orm.Executor` as their first argument and never
begin a transaction. That is the rule that makes atomicity decidable: a
repository cannot know whether its write is the whole of an operation or a third
of it, and one that quietly opened its own transaction would make the question
unanswerable from the outside.

There is no hidden transaction, no ambient one and no global database handle.

## Errors: three sentinels, and nothing from PostgreSQL

`domain.ErrNotFound`, `domain.ErrConflict`, `domain.ErrInvalid`. The store
translates into them by reading the **SQLSTATE**, never the message:

```go
case "23505": return fmt.Errorf("%s: %w", doing, domain.ErrConflict)
```

PostgreSQL's unique-violation message is `Key (email)=(someone@example.com)
already exists`. It contains a user's data, and a handler that echoed
`err.Error()` would publish it to whoever guessed the address. The response
carries a fixed sentence per category; the detail goes to the log.
`TestAPI_errorsDoNotLeak` and the per-transport sweep in `frameworks_test.go`
check this against a real constraint violation.

## Health: three endpoints, because they answer three questions

| Endpoint | Question | Cost | Failing means |
|---|---|---|---|
| `GET /livez` | is the process wedged? | touches nothing | restart me |
| `GET /readyz` | can it serve? | one statement | stop sending traffic |
| `GET /admin/db-health` | what is actually going on? | schema + migrations | a human should look |

Liveness must not touch the database. A database outage is not a reason to
restart every instance, and a liveness probe wired to the database turns one
outage into a restart loop. `TestHealth_livezNeverTouchesTheDatabase` builds the
handler with no database at all, so a query would have to panic.

The deep report is read-only, and there is a test that snapshots the schema
before and after to say so.

## Shutdown, in one order

```go
func (s *Server) Shutdown() error {
    defer s.pool.Close()                 // 2. and only then the database
    ...
    if err := s.http.Shutdown(ctx); err != nil { ... }  // 1. drain first
}
```

Reversing those two lines turns a clean shutdown into a burst of 500s from
requests that were almost finished. `TestShutdown_inFlightRequestFinishesWithADatabase`
proves the order by putting a request in flight — headers sent, body half
written, so the handler is blocked in `json.Decode` with no connection acquired —
and then shutting down. With the order reversed the request comes back 500; with
it right, 201.

The shutdown context is deliberately **not** the application's. That one has
just been cancelled, and passing it here would ask the server to drain with a
deadline that has already expired.

## Fiber needs one paragraph of thought

`c.Context()` returns a `*fasthttp.RequestCtx`. It satisfies `context.Context`,
which makes it look like the thing to hand to the database. It is not: fasthttp
pools those values and reuses them for later requests once the handler returns.

So a middleware installs a real context derived from the application's base
context, with a deadline, and handlers pass `c.UserContext()`:

```go
func (a *API) withContext(c *fiber.Ctx) error {
    ctx, cancel := context.WithTimeout(a.base, a.timeout)
    defer cancel()
    c.SetUserContext(ctx)
    return c.Next()
}
```

What this does **not** give you is cancellation when the client disconnects.
Fiber v2 does not surface that, and the example does not pretend otherwise. The
deadline is the bound that exists.

## Observability: one tracer, two destinations

An executor carries one tracer. Wrapping twice —
`orm.Traced(orm.Traced(pool, a), b)` — silently keeps only the outer one. To
reach two places, use one tracer that forwards:

```go
ex := orm.Traced(pool, observability.New(log, observability.Config{
    LogSQL: cfg.LogSQL,
    Traces: tp.Tracer("my-service"),
}))
```

Bind values never reach an event. `TestObservability_noBindValueReachesTelemetry`
writes passwords, tokens and email addresses through inserts, queries,
transactions and failures, then searches every log line and every span attribute
for them.
