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

## Patterns that appear in all four

The layout changes; these do not.

### Wiring, once, at startup

```go
func main() {
    ctx := context.Background()

    cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
    if err != nil {
        log.Fatal(err)
    }
    pool, err := pgxpool.NewWithConfig(ctx, cfg)
    if err != nil {
        log.Fatal(err)
    }
    defer pool.Close()

    db := domain.New(pool)
    svc := service.New(db)

    log.Fatal(http.ListenAndServe(":8080", routes(svc)))
}
```

Everything below `main` receives what it needs. Nothing reaches for a package
variable, which is why any of it can be tested against a different database
without a build tag.

### The service takes the handle, not the pool

```go
type Service struct {
    db *domain.DB
}

func New(db *domain.DB) *Service { return &Service{db: db} }
```

A service holding a `*pgxpool.Pool` would have to build the handle itself, and
then a transaction could not be passed into it — which is the next pattern.

### A transaction that spans several stores

```go
func (s *Service) Checkout(ctx context.Context, cart Cart) error {
    return s.db.Tx(ctx, func(tx *domain.DB) error {
        order, err := tx.Orders.Insert(ctx, Order{UserID: cart.UserID})
        if err != nil {
            return err
        }
        for _, line := range cart.Lines {
            if _, err := tx.Items.Insert(ctx, Item{OrderID: order.ID, SKU: line.SKU}); err != nil {
                return err
            }
        }
        return nil
    })
}
```

`tx` is a different handle from `s.db`, so a call that accidentally used the
outer one is visible in review rather than silently outside the transaction.

### Making a function work inside or outside a transaction

Take the handle as a parameter and let the caller decide:

```go
func reserve(ctx context.Context, db *domain.DB, sku string, n int32) error {
    _, err := db.Inventory.Update().
        Set(Inventory.OnHand.SetExpr(Inventory.OnHand.Sub(n))).
        Where(Inventory.SKU.Eq(sku)).
        Where(Inventory.OnHand.Gte(n)).
        Exec(ctx)
    return err
}
```

Called with `s.db` it is its own statement; called with `tx` inside `Tx` it joins
the transaction. Nothing about the function changes.

### An executor decorated once

```go
db := domain.New(orm.Traced(pool, ormslog.New(logger)))
```

`orm.Traced` wraps the executor, so every statement issued through the handle is
traced and nothing below this line mentions telemetry. A transaction started from
it inherits the decoration.

### Ports that name what the core needs

```go
package core

type UserStore interface {
    ByID(ctx context.Context, id int64) (User, error)
    Save(ctx context.Context, u User) error
}
```

### And an adapter that satisfies it

```go
package postgres

type UserStore struct{ db *domain.DB }

func (s UserStore) ByID(ctx context.Context, id int64) (core.User, error) {
    row, err := s.db.Users.Query().Where(Users.ID.Eq(id)).One(ctx)
    if err != nil {
        return core.User{}, err
    }
    return core.User{ID: row.ID, Email: row.Email}, nil
}
```

The translation between the row type and the domain type is the adapter's whole
job. Skipping it means the core's type is whatever the table happens to be, and
the port stops being a boundary.

### Mapping database errors at the boundary

```go
func (s UserStore) ByID(ctx context.Context, id int64) (core.User, error) {
    row, err := s.db.Users.Query().Where(Users.ID.Eq(id)).One(ctx)
    switch {
    case errors.Is(err, orm.ErrNotFound):
        return core.User{}, core.ErrNotFound
    case err != nil:
        return core.User{}, fmt.Errorf("loading user %d: %w", id, err)
    }
    return core.User{ID: row.ID, Email: row.Email}, nil
}
```

The core should not import `orm` to know a row was missing. One translation here
keeps that true.

### Paging that does not leak the cursor into the domain

```go
type Page[T any] struct {
    Items  []T
    Cursor string
}
```

### Context all the way down

```go
func (s *Service) List(ctx context.Context, f Filter) ([]core.User, error) {
    return s.store.Search(ctx, f)
}
```

Every ORM call takes a context and none of them stores one. A cancelled request
stops the query it started, which is only true if the context was threaded rather
than replaced with `context.Background()` somewhere in the middle.

### A background worker gets its own handle

```go
func worker(ctx context.Context, db *domain.DB) {
    tick := time.NewTicker(time.Minute)
    defer tick.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-tick.C:
            if err := sweep(ctx, db); err != nil {
                log.Error("sweep", "err", err)
            }
        }
    }
}
```

Sharing the pool is right; sharing a transaction is not. A worker that took a
`tx` would hold it open between ticks.

### Health checks that answer different questions

```go
mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
    if rep := ormhealth.Quick(r.Context(), pool); !rep.OK() {
        http.Error(w, rep.String(), http.StatusServiceUnavailable)
        return
    }
    w.WriteHeader(http.StatusOK)
})
```

```go
mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
    rep := ormhealth.Deep(r.Context(), pool, ormhealth.WithSchemaCheck("public"))
    if !rep.OK() {
        http.Error(w, rep.String(), http.StatusServiceUnavailable)
        return
    }
    w.WriteHeader(http.StatusOK)
})
```

`Quick` asks whether the database answers. `Deep` asks whether it is the database
this build expects — including whether the migrations are applied. Pointing a
liveness probe at the deep one restarts a healthy process because a migration is
pending, which is the opposite of what you wanted.

### Shutdown in the order that matters

```go
srv := &http.Server{Addr: ":8080", Handler: routes(svc)}

go func() {
    <-ctx.Done()
    shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
    defer cancel()
    _ = srv.Shutdown(shutdown)   // stop accepting, let in-flight requests finish
    pool.Close()                 // only then close the pool
}()
```

Closing the pool first turns every in-flight request into an error, which is a
worse outcome than the twenty seconds.

### Multi-tenancy by schema

```yaml
packages:
  - path: ./internal/tenanta/domain
    schema: tenant_a
    output: same
  - path: ./internal/tenantb/domain
    schema: tenant_b
    output: same
```

Two packages, two sets of descriptors, one binary. A query built from one cannot
be run against the other, because the descriptors carry the schema.

### A read replica for reporting

```go
reports := domain.New(replicaPool)

rows, err := orm.Select(reports.Orders, monthly).GroupBy(Orders.Status).All(ctx)
```

A second handle over a second pool. Nothing else changes, and a write attempted
through it fails at the server rather than silently going to the wrong place.

### Testing the core without a database

```go
type fakeUsers struct{ byID map[int64]core.User }

func (f fakeUsers) ByID(_ context.Context, id int64) (core.User, error) {
    u, ok := f.byID[id]
    if !ok {
        return core.User{}, core.ErrNotFound
    }
    return u, nil
}
```

This is what the port bought. The core's tests are a map and no server.

### Testing the adapter against a real one

```go
func TestUserStore(t *testing.T) {
    ex := ormtest.Tx(t, pool)
    store := postgres.UserStore{DB: domain.New(ex)}
    // ...
}
```

The adapter is the layer whose whole job is talking to PostgreSQL, so its tests
talk to PostgreSQL. Faking the database here would test the fake.

### Configuration read once

```go
type Config struct {
    DatabaseURL string
    MaxConns    int32
    Addr        string
}
```

A struct filled at startup and passed down, rather than `os.Getenv` scattered
through the packages that happen to need a value.

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
