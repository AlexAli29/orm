# Project architectures

> Four layouts that work, and what each one is actually for.

Source: https://ormgo.vercel.app/en/docs/cookbook/architecture/
Symbols: https://ormgo.vercel.app/api/orm.txt — the generated list of every exported name.

---
The repository ships all four as compiling, tested modules. What follows is the shape and the reasoning; the code is under `examples/`.

## 1. Flat — a small service

```text
cmd/api/main.go
internal/domain/entities.go     ← you write this
internal/domain/orm_*.gen.go    ← generated beside it
internal/http/handlers.go
orm.yaml
migrations/
```

Handlers take `*domain.DB` directly. There is no repository layer, because at this size a repository interface with one implementation is a file you maintain for nothing.

Reach for something else when handlers start containing business rules, or when you want to test them without a database.

## 2. Hexagonal — ports and adapters

```text
internal/core/            ← entities, services, port interfaces. No SQL, no HTTP.
internal/adapters/postgres/
internal/adapters/http/
cmd/api/
```

The core defines what it needs:

```go
package core

type UserStore interface {
    ByID(ctx context.Context, id int64) (User, error)
    Save(ctx context.Context, u User) error
}
```

The adapter implements it with the ORM. The core imports neither `orm` nor `net/http`, so its tests need neither.

**The rule that makes it work:** interfaces are defined where they are *used*, not where they are implemented. A `UserStore` in the adapter package is a `UserStore` the core cannot depend on without depending on the adapter.

Reach for it when the domain has real rules, or when you have more than one transport.

## 3. Modular monolith — bounded contexts

```text
internal/billing/     domain/ · store/ · service.go · port.go
internal/catalog/     domain/ · store/ · service.go · port.go
internal/identity/    domain/ · store/ · service.go · port.go
cmd/api/main.go       ← wires them together
```

Each context owns its own schema — its own entities, its own `orm.yaml` package entry, sometimes its own PostgreSQL schema. Contexts talk through `port.go` and never import each other's `store/`.

```yaml
packages:
  - path: ./internal/billing/domain
    output: same
  - path: ./internal/catalog/domain
    output: same
```

Two contexts owning tables with the same name is fine, and the cross-schema tests exist to prove it: `billing.users` and `identity.users` produce separate descriptors, separate migration state and separate results.

This is the layout that survives being split into services later, because the seams are already there.

## 4. Production — the full stack

The `examples/production` module is the reference for everything an actual deployment needs:

- **Four HTTP transports** — net/http, chi, gin and fiber — over one service layer, to prove the service layer does not know about any of them.
- **Observability** wired once at startup: `orm.Traced(pool, tracer)` and nothing below it mentions telemetry.
- **Health checks** through `ormhealth`, including whether migrations are actually applied.
- **Graceful shutdown**, in the order that matters: stop accepting, drain, then close the pool.

```go
func main() {
    pool, err := pgxpool.NewWithConfig(ctx, cfg)
    // ...
    db := domain.New(orm.Traced(pool, observability.New(log, obsCfg)))
    svc := service.New(db)
    srv := server.New(httpapi.Routes(svc))
    // ...
}
```

One call, at startup, on the executor. A transaction started from it inherits the tracing.

## Choosing

| | Flat | Hexagonal | Modular monolith | Production |
| --- | --- | --- | --- | --- |
| Domain rules | few | many | many | many |
| Transports | one | one or more | one or more | several |
| Team size | 1–3 | 2–6 | 5+ | any |
| Splitting later | painful | possible | designed for | designed for |

## Rules that hold in all four

**Generated code lives beside its entities.** `output: same` puts descriptors in the same package as the structs they describe, so an import cycle is impossible.

**One executor, passed down.** No global DB, no `init()` connection, no ambient transaction. What a function can reach is what it was given.

**Migrations are code review.** They are committed artifacts, planned by a command and read by a person before they run.

**The two CI commands are not optional:**

```bash
orm makemigrations --check
orm check --generated
```

The first fails when a struct changed and nobody planned it. The second fails when somebody planned and forgot to regenerate. Between them, the three representations cannot drift.
