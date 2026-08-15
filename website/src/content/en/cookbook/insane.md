---
title: Hard queries
description: The ones people reach for raw SQL to write — composed, typed, and still one statement.
---

Each of these is one SQL statement with one parameter list. None of them is assembled from strings.

These lean on the composition API rather than the entity query, because that is what they need: a derived table, a CTE, a lateral, a set operation. The vocabulary is small and it repeats. `orm.Rows` lists the columns a subquery exposes, `orm.Named` gives one of them a name, `orm.Sub` turns that into a derived table, `orm.Ref` reads a named column back out of it, and `orm.Cond` lifts an entity predicate into composed scope. Everything below is those five and the joins.

## Top-N per group

The classic. A window function inside a derived table, filtered outside it — because a window function cannot appear in a `WHERE`.

```go
rank := orm.Named("rn", orm.RowNumber().
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

### The same thing with a lateral, which is often faster

When the parent set is small and the child table is indexed on the join key, a
lateral beats ranking the whole child table and throwing most of it away:

```go
top := orm.Sub("top", orm.Rows(
    orm.Named("id", orm.Of(Orders.ID)),
    orm.Named("placed", orm.Of(Orders.Placed)),
).From(Orders.Source()).
    Where(orm.Eq(Orders.UserID, Users.ID)).
    OrderBy(orm.Of(Orders.Placed).Desc()).
    Limit(3))

orm.Compose(pool, shape).From(Users.Source()).LeftJoinLateral(top)
```

Two plans for one question. Measure rather than assume — `Explain` is right there.

## Running total

```go
running := orm.Named("running", orm.SumInt64[orm.Composed, int64](orm.Of(Orders.Total)).Over(orm.Window().
    OrderBy(orm.Of(Orders.Placed).Asc()).
    Rows(orm.UnboundedPreceding(), orm.CurrentRow())))
```

### Running total that resets each month

```go
month := orm.DateTrunc(orm.Month, Orders.Placed)

perMonth := orm.Named("mtd", orm.SumInt64[orm.Composed](orm.Of(Orders.Total)).Over(orm.Window().
    PartitionBy(month).
    OrderBy(orm.Of(Orders.Placed).Asc()).
    Rows(orm.UnboundedPreceding(), orm.CurrentRow())))
```

The partition is the reset. Nothing else changes.

### Balance after each entry

The shape a bank statement needs — every row carrying the balance as of itself:

```go
balance := orm.Named("balance", orm.SumInt64[orm.Composed](orm.Of(Entries.Cents)).Over(orm.Window().
    PartitionBy(orm.Of(Entries.AccountID)).
    OrderBy(orm.Of(Entries.At).Asc(), orm.Of(Entries.ID).Asc()).
    Rows(orm.UnboundedPreceding(), orm.CurrentRow())))
```

The `ID` in the ordering is not decoration. Two entries in the same microsecond
would otherwise get an order the plan chose, and a statement whose balances
change between runs is worse than one that is merely wrong.

## Gaps and islands

Consecutive runs of activity, found by the difference between a row number and the date.

```go
grp := orm.Named("grp", orm.Sub(
    orm.Of(Events.Day),
    orm.RowNumber().OrderBy(orm.Of(Events.Day).Asc()),
))

islands := orm.Sub("islands", orm.Rows(
    orm.Named("day", orm.Of(Events.Day)),
    grp,
).From(Events.Source()))

// then group by grp and take min(day), max(day)
```

### The streak, as a number

Grouping the islands gives the run lengths — a login streak, an uptime window, a
consecutive-days-shipped count:

```go
orm.Compose(pool, streaks).
    From(islands).
    GroupBy(orm.Ref(islands, grp)).
    Having(orm.Count[orm.Composed]().Gte(3)).
    OrderBy(orm.Ref(islands, grp).Asc())
```

### Gaps: the periods where nothing happened

The complement of the same idea — each row paired with the previous one, and the
distance between them:

```go
prev := orm.Named("prev", orm.Lag(Readings.At).Over(orm.Window().
    PartitionBy(orm.Of(Readings.SensorID)).
    OrderBy(orm.Of(Readings.At).Asc())))

gaps := orm.Sub("gaps", orm.Rows(
    orm.Named("sensor_id", orm.Of(Readings.SensorID)),
    orm.Named("at", orm.Of(Readings.At)),
    prev,
).From(Readings.Source()))
```

A sensor that should report every minute and has a two-hour gap is a fault, and
this is the query that finds it without pulling a year of readings into Go.

## Recursive hierarchy

An org chart, to any depth, in one statement:

```go
anchor := orm.Rows(
    orm.Named("id", orm.Of(Employees.ID)),
    orm.Named("manager_id", orm.Opt(Employees.ManagerID)),
    orm.Named("depth", orm.Val(0)),
).From(Employees.Source()).Where(orm.Cond(Employees.ManagerID.IsNull()))

tree := orm.RecursiveCTE("tree", anchor, func(self *orm.Source) orm.Term {
    return orm.Rows(
        orm.Named("id", orm.Of(Employees.ID)),
        orm.Named("manager_id", orm.Opt(Employees.ManagerID)),
        orm.Named("depth", orm.Ref(self, depth).Add(1)),
    ).From(Employees.Source()).
        Join(self, orm.Eq(Employees.ManagerID, orm.Ref(self, id)))
})
```

`UNION` appears here and only here — PostgreSQL's grammar requires it between a recursive CTE's anchor and its recursive term. It is not the general set-composition feature; that is [UNION ALL](/en/docs/union-all/).

### Everything under one node

The same walk started somewhere other than the root — a subtree, a folder, a
comment thread:

```go
anchor := orm.Rows(
    orm.Named("id", orm.Of(Categories.ID)),
    orm.Named("parent_id", orm.Opt(Categories.ParentID)),
).From(Categories.Source()).Where(orm.Cond(Categories.ID.Eq(root)))
```

### A materialised path, built on the way down

So a breadcrumb does not cost one query per level:

```go
tree := orm.RecursiveCTE("tree", anchor, func(self *orm.Source) orm.Term {
    return orm.Rows(
        orm.Named("id", orm.Of(Categories.ID)),
        orm.Named("path", orm.Concat(orm.Ref(self, path), orm.Val(" / "), orm.Of(Categories.Name))),
    ).From(Categories.Source()).
        Join(self, orm.Eq(Categories.ParentID, orm.Ref(self, id)))
})
```

### Bill of materials, with quantities multiplied down

Every recursion step multiplies by the parent's quantity, so a leaf's number is
what you actually have to buy:

```go
tree := orm.RecursiveCTE("bom", anchor, func(self *orm.Source) orm.Term {
    return orm.Rows(
        orm.Named("part_id", orm.Of(Assemblies.ChildID)),
        orm.Named("qty", orm.Ref(self, qty).Mul(orm.Of(Assemblies.Qty))),
    ).From(Assemblies.Source()).
        Join(self, orm.Eq(Assemblies.ParentID, orm.Ref(self, partID)))
})
```

## Correlated subquery in the select list

The most recent order per user, without a join:

```go
last := orm.Scalar[User, time.Time](
    db.Orders.Query().
        Where(orm.Eq(Orders.UserID, Users.ID)).
        OrderBy(Orders.Placed.Desc()).
        Limit(1),
)

var shape = orm.Project2(
    Users.Email, last,
    func(email string, at *time.Time) Row { return Row{email, at} },
)
```

A scalar subquery is always nullable, because no row yields NULL. The type says so.

### A count beside each row

```go
n := orm.Scalar[User, int64](
    db.Orders.Query().Where(orm.Eq(Orders.UserID, Users.ID)),
)
```

Convenient, and one subquery per row. When the list is long, a `GROUP BY` joined
once is the same answer for less — this shape earns its place on a page of
twenty rows, not on an export of two hundred thousand.

### Two correlated values without two subqueries

```go
stats := orm.Sub("stats", orm.Rows(
    orm.Named("user_id", orm.Of(Orders.UserID)),
    orm.Named("n", orm.Count[orm.Composed]()),
    orm.Named("last", orm.Max(Orders.Placed)),
).From(Orders.Source()).GroupBy(orm.Of(Orders.UserID)))

orm.Compose(pool, shape).
    From(Users.Source()).
    LeftJoin(stats, orm.Eq(Users.ID, orm.Ref(stats, userID)))
```

## Anti-join two ways

```go
// NOT EXISTS — usually the planner's favourite
db.Users.Query().Where(orm.NotExists(
    db.Orders.Query().Where(orm.Eq(Orders.UserID, Users.ID)),
))

// LEFT JOIN ... IS NULL, when you also want columns from the right side
orm.Compose(pool, shape).
    From(Users.Source()).
    LeftJoin(Orders.Source(), orm.Eq(Orders.UserID, Users.ID)).
    Where(orm.Opt(Orders.ID).IsNull())
```

### Semi-join: parents with at least one match, each parent once

```go
db.Users.Query().Where(orm.Exists[User](
    db.Orders.Query().Where(orm.And(
        orm.Eq(Orders.UserID, Users.ID),
        Orders.Status.Eq("paid"),
    )),
))
```

A join would give one row per matching order. `EXISTS` gives one row per user,
which is what "users who have paid" means.

### Why NOT IN is the one to avoid

```go
db.Users.Query().Where(orm.NotExists[User](
    db.Blocks.Query().Where(orm.Eq(Blocks.UserID, Users.ID)),
))
```

If a `NOT IN` subquery yields a single NULL it returns no rows at all — silently,
and only on the days the data contains one. `NOT EXISTS` has no such trapdoor,
which is why this page shows it and not the other.

## Lateral join

The two most recent orders per user, with the subquery correlating to the row on its left:

```go
recent := orm.Sub("recent", orm.Rows(
    orm.Named("id", orm.Of(Orders.ID)),
    orm.Named("placed", orm.Of(Orders.Placed)),
).From(Orders.Source()).
    Where(orm.Cond(orm.Eq(Orders.UserID, Users.ID))).
    OrderBy(orm.Of(Orders.Placed).Desc()).
    Limit(2))

orm.Compose(pool, shape).
    From(Users.Source()).
    LeftJoinLateral(recent)
```

### A per-row aggregate that a join cannot express

The spend of each customer inside a window that differs per row:

```go
window := orm.Sub("window", orm.Rows(
    orm.Named("spent", orm.SumInt64[orm.Composed](orm.Of(Orders.Total))),
).From(Orders.Source()).Where(orm.Cond(orm.And(
    orm.Eq(Orders.UserID, Users.ID),
    Orders.Placed.Between(from, to),
))))

orm.Compose(pool, shape).From(Users.Source()).LeftJoinLateral(window)
```

### Inner lateral, to drop rows with no match

```go
orm.Compose(pool, shape).From(Users.Source()).JoinLateral(recent)
```

`LeftJoinLateral` keeps users with no orders and gives them NULLs.
`JoinLateral` drops them. The choice is the same one an ordinary join makes.

## Pivot

Counts by status, as columns rather than rows:

```go
var pivot = orm.Project3(
    Orders.UserID,
    orm.Count[Order]().Filter(Orders.Status.Eq("paid")),
    orm.Count[Order]().Filter(Orders.Status.Eq("refunded")),
    func(id int64, paid, refunded int64) Pivot { return Pivot{id, paid, refunded} },
)

orm.Select(db.Orders, pivot).GroupBy(Orders.UserID)
```

`FILTER` beats `CASE WHEN` here: it says what it means and the planner reads it better.

### Sums per bucket, not just counts

```go
var revenue = orm.Project4(
    Sales.Region,
    orm.SumInt32(Sales.Cents).Filter(Sales.Channel.Eq("web")),
    orm.SumInt32(Sales.Cents).Filter(Sales.Channel.Eq("retail")),
    orm.SumInt32(Sales.Cents).Filter(Sales.Channel.Eq("partner")),
    func(region string, web, retail, partner *int64) Revenue {
        return Revenue{region, web, retail, partner}
    },
)
```

The columns are fixed at compile time, which is the honest constraint: SQL cannot
produce a column set that depends on the data, and neither can a typed API. If
the buckets are discovered at run time, the answer is rows, and the pivot happens
in the consumer.

## Set composition across relations

A table, a view and a materialized view in one result — legal because the projections are identical:

```go
shape := orm.Project2(
    orm.Of(Users.ID), orm.Of(Users.Email),
    func(id uuid.UUID, email string) Row { return Row{id, email} },
)

email := orm.Named("email", orm.Of(Users.Email))

rows, err := orm.UnionAll[Row](
    orm.Compose(pool, shape).From(Users.Source()),
    orm.Compose(pool, shape).From(ActiveUsers.Source()),
    orm.Compose(pool, shape).From(UserSummaries.Source()),
).OrderBy(email.Asc()).Limit(50).All(ctx)
```

No special handling by source kind. A read source is a read source.

### A merged activity feed

Three tables that share nothing but a timestamp and a label:

```go
rows, err := orm.UnionAll[Item](
    orm.Compose(pool, feed).From(Posts.Source()),
    orm.Compose(pool, feed).From(Comments.Source()),
    orm.Compose(pool, feed).From(Follows.Source()),
).OrderBy(at.Desc()).Limit(50).All(ctx)
```

The `ORDER BY` and `LIMIT` apply to the union, not to a branch — one sorted feed,
not three sorted lists concatenated.

### Rows in one table and not the other, both directions

```go
orm.UnionAll[Diff](
    orm.Compose(pool, diff).From(Expected.Source()).Where(orm.NotExists[orm.Composed](
        orm.Compose(pool, one).From(Actual.Source()).Where(orm.Eq(Actual.Key, Expected.Key)))),
    orm.Compose(pool, diff).From(Actual.Source()).Where(orm.NotExists[orm.Composed](
        orm.Compose(pool, one).From(Expected.Source()).Where(orm.Eq(Expected.Key, Actual.Key)))),
)
```

A reconciliation report — what is missing and what is extra — in one statement.

## Self-join through an alias

```go
mgr := Employees.As("mgr")

orm.Compose(pool, shape).
    From(Employees.Source()).
    LeftJoin(mgr.Source(), orm.Eq(mgr.ID, Employees.ManagerID))
```

`As` returns a second source, and a descriptor built from one cannot be used against the other. That is what makes a self-join safe rather than a naming exercise.

### Finding duplicates by comparing a table to itself

```go
other := Contacts.As("other")

orm.Compose(pool, pairs).
    From(Contacts.Source()).
    Join(other.Source(), orm.And(
        orm.Eq(Contacts.Email, other.Email),
        orm.Cond(Contacts.ID.Lt(other.ID)),
    ))
```

The `Lt` is what stops every pair appearing twice and every row matching itself.

## Deduplication

### Keep the newest row per key

```go
rank := orm.Named("rn", orm.RowNumber().
    PartitionBy(orm.Of(Imports.ExternalID)).
    OrderBy(orm.Of(Imports.SeenAt).Desc()))

deduped := orm.Sub("deduped", orm.Rows(
    orm.Named("id", orm.Of(Imports.ID)),
    orm.Named("external_id", orm.Of(Imports.ExternalID)),
    rank,
).From(Imports.Source()))

orm.Compose(pool, shape).From(deduped).Where(orm.Ref(deduped, rank).Eq(1))
```

### Or with DISTINCT ON, which is shorter

```go
orm.Compose(pool, shape).
    From(Imports.Source()).
    DistinctOn(orm.Of(Imports.ExternalID)).
    OrderBy(orm.Of(Imports.ExternalID).Asc(), orm.Of(Imports.SeenAt).Desc())
```

Same answer. The window version ports to other databases; this one is faster and
says what it means. Since this library is PostgreSQL-only, prefer this.

### Delete the duplicates rather than filter them

```go
db.Imports.Delete().Where(orm.InSub(
    Imports.ID,
    orm.Compose(pool, ids).From(deduped).Where(orm.Ref(deduped, rank).Gt(1)),
)).Exec(ctx)
```

## Analytics shapes

### A histogram

```go
bucket := orm.Named("bucket", orm.Fn[orm.Composed, int32]("width_bucket",
    orm.ArgOf(Response.Millis), orm.ArgValue(0), orm.ArgValue(1000), orm.ArgValue(10)))

orm.Compose(pool, histogram).
    From(Response.Source()).
    GroupBy(bucket).
    OrderBy(bucket.Asc())
```

### Cohort retention

```go
cohort := orm.Named("cohort", orm.DateTrunc(orm.Month, Users.CreatedAt))
active := orm.Named("active", orm.DateTrunc(orm.Month, Events.At))

orm.Compose(pool, retention).
    From(Users.Source()).
    Join(Events.Source(), orm.Eq(Events.UserID, Users.ID)).
    GroupBy(cohort, active).
    OrderBy(cohort.Asc(), active.Asc())
```

Two truncations and a group. The grid the result forms is the cohort table, and
the pivot into columns belongs in the consumer.

### Nearest neighbour, by distance

```go
distance := postgis.OfGeog(Stops.Spot).Distance(postgis.GeogValue[Stop](here))

orm.Select(db.Stops, nearest).
    Where(postgis.OfGeog(Stops.Spot).DWithin(postgis.GeogValue[Stop](here), 2000)).
    OrderBy(distance.Asc()).
    Limit(5)
```

The `DWithin` before the sort is what lets the index do the work.

## Writing, composed

### Delete and read what was deleted, in one statement

```go
archived := orm.WritingCTE("archived", db.Events.Delete().
    Where(Events.At.Lt(cutoff)))

orm.Compose(pool, shape).With(archived).From(archived)
```

Doing it in two statements means the rows can change between them.

### Learn which rows actually changed

```go
changed, err := orm.UpdateReturning(
    db.Prices.Update().
        Set(Prices.Cents.Set(cents)).
        Where(Prices.SKU.Eq(sku)).
        Where(Prices.Cents.Ne(cents)),
    priceShape,
).All(ctx)
```

The second `Where` is the trick: an update that would change nothing matches
nothing, so the returned rows are exactly the ones that moved. That is the set
worth publishing an event for.

## Batched upsert of a whole set

```go
err := db.Tx(ctx, func(tx *domain.DB) error {
    for chunk := range slices.Chunk(rows, 1000) {
        if _, err := tx.Prices.InsertMany(ctx, chunk,
            orm.OnConflict(Prices.SKU).DoUpdate(Prices.Amount),
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

### Comparing two spellings of one question

```go
a, _ := withNotExists.Explain(ctx)
b, _ := withLeftJoin.Explain(ctx)
```

The anti-join above has two forms and this page declines to say which is faster,
because that depends on your row counts and your indexes. This is how you find
out, on your data, in about a minute.

### Checking the SQL is what you meant

```go
sql, args, err := q.SQL()
```

No values are interpolated into that string — the arguments come back beside it,
which is the same thing the server receives.
