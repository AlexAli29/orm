---
title: Hard queries
description: The ones people reach for raw SQL to write — composed, typed, and still one statement.
---

Each of these is one SQL statement with one parameter list. None of them is assembled from strings.

## Top-N per group

The classic. A window function inside a derived table, filtered outside it — because a window function cannot appear in a `WHERE`.

```go
rank := orm.Named("rn", orm.RowNumber[orm.Composed]().
    PartitionBy(orm.Of(Orders.UserID)).
    OrderBy(orm.Of(Orders.Placed).Desc()))

ranked := orm.Sub("ranked", orm.Rows(
    orm.Named("id", orm.Of(Orders.ID)),
    orm.Named("user_id", orm.Of(Orders.UserID)),
    orm.Named("placed", orm.Of(Orders.Placed)),
    rank,
).From(Orders.Source()))

rows, err := orm.Compose(pool, shape).
    From(ranked).
    Where(orm.Ref(ranked, rank).Lte(3)).
    OrderBy(orm.Ref(ranked, userID).Asc()).
    All(ctx)
```

## Running total

```go
running := orm.Named("running", orm.SumOver(orm.Of(Orders.Total)).
    OrderBy(orm.Of(Orders.Placed).Asc()).
    Rows(orm.UnboundedPreceding, orm.CurrentRow))
```

## Gaps and islands

Consecutive runs of activity, found by the difference between a row number and the date.

```go
grp := orm.Named("grp", orm.Sub(
    orm.Of(Events.Day),
    orm.RowNumber[orm.Composed]().OrderBy(orm.Of(Events.Day).Asc()),
))

islands := orm.Sub("islands", orm.Rows(
    orm.Named("day", orm.Of(Events.Day)),
    grp,
).From(Events.Source()))

// then group by grp and take min(day), max(day)
```

## Recursive hierarchy

An org chart, to any depth, in one statement:

```go
anchor := orm.Rows(
    orm.Named("id", orm.Of(Employees.ID)),
    orm.Named("manager_id", orm.Opt(Employees.ManagerID)),
    orm.Named("depth", orm.Lit(0)),
).From(Employees.Source()).Where(orm.Cond(Employees.ManagerID.IsNull()))

tree := orm.RecursiveCTE("tree", anchor, func(self *orm.Source) orm.Term {
    return orm.Rows(
        orm.Named("id", orm.Of(Employees.ID)),
        orm.Named("manager_id", orm.Opt(Employees.ManagerID)),
        orm.Named("depth", orm.Add(orm.Ref(self, depth), orm.Lit(1))),
    ).From(Employees.Source()).
        Join(self, orm.Eq(Employees.ManagerID, orm.Ref(self, id)))
})
```

`UNION` appears here and only here — PostgreSQL's grammar requires it between a recursive CTE's anchor and its recursive term. It is not the general set-composition feature; that is [UNION ALL](/en/docs/union-all/).

## Correlated subquery in the select list

The most recent order per user, without a join:

```go
last := orm.Scalar[User, time.Time](
    db.Orders.Query().
        Where(Orders.UserID.EqCol(Users.ID)).
        OrderBy(Orders.Placed.Desc()).
        Limit(1),
)

var shape = orm.Project2(
    Users.Email, last,
    func(email string, at *time.Time) Row { return Row{email, at} },
)
```

A scalar subquery is always nullable, because no row yields NULL. The type says so.

## Anti-join two ways

```go
// NOT EXISTS — usually the planner's favourite
db.Users.Query().Where(orm.NotExists(
    db.Orders.Query().Where(Orders.UserID.EqCol(Users.ID)),
))

// LEFT JOIN ... IS NULL, when you also want columns from the right side
orm.Compose(pool, shape).
    From(Users.Source()).
    LeftJoin(Orders.Source(), orm.Eq(Orders.UserID, Users.ID)).
    Where(orm.Opt(Orders.ID).IsNull())
```

## Lateral join

The two most recent orders per user, with the subquery correlating to the row on its left:

```go
recent := orm.Sub("recent", orm.Rows(
    orm.Named("id", orm.Of(Orders.ID)),
    orm.Named("placed", orm.Of(Orders.Placed)),
).From(Orders.Source()).
    Where(orm.Cond(Orders.UserID.EqCol(Users.ID))).
    OrderBy(orm.Of(Orders.Placed).Desc()).
    Limit(2))

orm.Compose(pool, shape).
    From(Users.Source()).
    LeftJoinLateral(recent, orm.True())
```

## Pivot

Counts by status, as columns rather than rows:

```go
var pivot = orm.Project3(
    Orders.UserID,
    orm.CountFilter[Order](Orders.Status.Eq("paid")),
    orm.CountFilter[Order](Orders.Status.Eq("refunded")),
    func(id int64, paid, refunded int64) Pivot { return Pivot{id, paid, refunded} },
)

orm.Select(db.Orders, pivot).GroupBy(Orders.UserID)
```

`FILTER` beats `CASE WHEN` here: it says what it means and the planner reads it better.

## Set composition across relations

A table, a view and a materialized view in one result — legal because the projections are identical:

```go
shape := orm.Project2(/* uuid, text */)

email := orm.Named("email", orm.Of(Users.Email))

rows, err := orm.UnionAll[Row](
    orm.Compose(pool, shape).From(Users.Source()),
    orm.Compose(pool, shape).From(ActiveUsers.Source()),
    orm.Compose(pool, shape).From(UserSummaries.Source()),
).OrderBy(email.Asc()).Limit(50).All(ctx)
```

No special handling by source kind. A read source is a read source.

## Self-join through an alias

```go
mgr := Employees.As("mgr")

orm.Compose(pool, shape).
    From(Employees.Source()).
    LeftJoin(mgr.Source(), orm.Eq(mgr.ID, Employees.ManagerID))
```

`As` returns a second source, and a descriptor built from one cannot be used against the other. That is what makes a self-join safe rather than a naming exercise.

## Batched upsert of a whole set

```go
err := db.Tx(ctx, func(tx *domain.DB) error {
    for chunk := range slices.Chunk(rows, 1000) {
        if _, err := tx.Prices.InsertMany(ctx, chunk,
            orm.OnConflict(Prices.SKU).DoUpdate(
                orm.Assign(Prices.Amount, orm.Excluded(Prices.Amount)),
            ),
        ); err != nil {
            return err
        }
    }
    return nil
})
```

## Reading the plan before trusting any of it

```go
plan, err := q.Explain(ctx)              // EXPLAIN, never runs the statement
plan, err := q.ExplainAnalyze(ctx)       // runs it, and the name says so
report, err := q.PerformanceReport(ctx)  // plan, shape and fingerprint
```

The two names differ because the behaviours differ dangerously. Nothing here recommends anything: PostgreSQL plans, and what to change needs a whole workload rather than one statement.
