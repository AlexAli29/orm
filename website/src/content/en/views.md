---
title: Views and materialized views
description: Read sources the ORM treats as first class, and the refresh lifecycle.
---

## Views

A view is declared like a table, plus the definition and what it depends on:

```go
//orm:view public.user_orders
//orm:definition `SELECT u.id AS user_id, o.id AS order_id, o.label
//                  FROM users u JOIN orders o ON o.user_id = u.id`
//orm:depends-on public.users
//orm:depends-on public.orders
type UserOrder struct {
    UserID  int64
    OrderID int64
    Label   string
}
```

`depends-on` is what orders the migration plan. A view created before the table it selects from is a migration that fails on a clean database and works on yours.

## View columns are nullable

A view's output nullability is not provable from the definition, so every column comes back as the nullable descriptor:

```go
UserOrders.UserID // NullOrdCol[UserOrder, int64], not OrdCol
```

That is honest rather than conservative: `SELECT ... FROM a LEFT JOIN b` can produce NULL in a column whose base is `NOT NULL`, and the view does not record which.

## Materialized views

```go
//orm:materialized-view public.user_summaries
//orm:definition `SELECT user_id, count(*) AS orders
//                  FROM user_orders GROUP BY user_id`
//orm:depends-on public.user_orders
//orm:index user_summaries_key (UserID) unique
type UserSummary struct {
    UserID int64
    Orders int64
}
```

`db.UserSummaries` is a `MaterializedViewRepo`. It offers what a view offers plus `Refresh`, and no writes — PostgreSQL has no `INSERT` for a materialized view, so there is nothing that could be generated even in principle.

## Refresh

```go
err := db.UserSummaries.Refresh(ctx)                     // REFRESH MATERIALIZED VIEW
err := db.UserSummaries.Refresh(ctx, orm.Concurrently()) // ... CONCURRENTLY
err := db.UserSummaries.Refresh(ctx, orm.WithNoData())   // ... WITH NO DATA
```

`CONCURRENTLY` needs a unique index over a non-partial set of plain columns. The generator works that out and writes the answer into the descriptor, so the check costs no round trip:

```text
orm: Refresh public.user_summaries: CONCURRENTLY needs a unique index over plain
columns covering every row, and this materialized view has none. A partial or
expression unique index does not qualify. Add one, or refresh without Concurrently
```

## The two ways that answer goes stale

The eligibility answer is a fact about the schema **at generation time**, and the schema keeps moving. The two resulting states fail in opposite directions, and knowing which you are in is the whole reason to regenerate.

**Behind the database.** The index arrives; the descriptor has not been regenerated. The code refuses locally and sends nothing. Nothing is broken — something is unavailable. `orm check --generated` reports it.

**Ahead of the database.** The index goes; the descriptor still says yes. The statement goes out and PostgreSQL refuses it:

```go
if err := db.UserSummaries.Refresh(ctx, orm.Concurrently()); err != nil {
    var pge *pgconn.PgError
    if errors.As(err, &pge) && pge.Code == "55000" {
        // object not in prerequisite state — the index is gone
    }
}
```

The error arrives as PostgreSQL's own. Rewriting it into a generic "refresh failed" would lose the SQLSTATE and everything a caller could branch on.

## Concurrent refresh, chosen deterministically

When several indexes qualify, the lowest name wins. It has to be deterministic: the generated descriptor and the fingerprint computed from it must name the same index on two runs over one schema, or every regeneration produces a diff.

## Worked examples

### A reporting view

```go
//orm:view analytics.monthly_revenue
//orm:definition `SELECT date_trunc('month', issued_at) AS month,
//                       plan, sum(amount_cents) AS cents
//                  FROM billing.invoices GROUP BY 1, 2`
//orm:depends-on billing.invoices
type MonthlyRevenue struct {
    Month time.Time
    Plan  string
    Cents int64
}
```

Every column comes back nullable, because a view's output nullability is not
provable — `sum` over no rows is NULL and the definition does not record which
columns can be.

### A materialized view that refreshes concurrently

```go
//orm:materialized-view analytics.search_index
//orm:definition `SELECT p.id, p.name, p.tags FROM catalog.products p WHERE p.listed`
//orm:depends-on catalog.products
//orm:index search_index_id_key (ID) unique
type SearchRow struct {
    ID   int64
    Name string
    Tags []string
}
```

The unique index over one plain column is what makes `Concurrently` possible.
Without it the refresh takes an exclusive lock and the site stops serving while
it runs.

```go
if err := db.SearchRows.Refresh(ctx, orm.Concurrently()); err != nil {
    var pge *pgconn.PgError
    if errors.As(err, &pge) && pge.Code == "55000" {
        // the index is gone; regenerate and redeploy
    }
    return err
}
```

### Refreshing on a schedule

```go
func refreshLoop(ctx context.Context, db *domain.DB) {
    t := time.NewTicker(5 * time.Minute)
    defer t.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-t.C:
            if err := db.SearchRows.Refresh(ctx, orm.Concurrently()); err != nil {
                log.Printf("refresh: %v", err)
            }
        }
    }
}
```

A concurrent refresh does not block readers, so a five-minute tick is a cost in
CPU rather than in availability.
