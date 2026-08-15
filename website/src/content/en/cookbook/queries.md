---
title: Query recipes
description: The everyday shapes, written out — filtering, paging, aggregation, joins, windows, upserts, search.
---

Every recipe here is complete enough to paste. `db` is the generated `*domain.DB`; `Users`, `Orders` and friends are the generated descriptors. The domains change from recipe to recipe on purpose — the shape is the point, and a shape you have only ever seen applied to `users` is one you have to translate before you can use it.

## Filtering

### Optional filters from a request

```go
func (s *Store) Search(ctx context.Context, f Filter) ([]User, error) {
    q := s.db.Users.Query()
    if f.Email != "" {
        q = q.Where(Users.Email.ILike("%" + f.Email + "%"))
    }
    if f.Active != nil {
        q = q.Where(Users.Active.Eq(*f.Active))
    }
    if !f.Since.IsZero() {
        q = q.Where(Users.CreatedAt.Gte(f.Since))
    }
    return q.OrderBy(Users.CreatedAt.Desc()).Limit(f.Limit).All(ctx)
}
```

No filters means no `WHERE` clause at all, not `WHERE TRUE`.

### Either/or

```go
db.Users.Query().Where(orm.Or(
    Users.Email.ILike("%@example.com"),
    Users.Email.ILike("%@example.org"),
))
```

### Anything but

```go
db.Users.Query().Where(orm.Not(Users.ID.In(banned...)))
```

### NULL versus empty

```go
db.Users.Query().Where(Users.Bio.IsNull())            // never set
db.Users.Query().Where(Users.Bio.Eq(""))              // set to empty
db.Users.Query().Where(orm.Or(
    Users.Bio.IsNull(), Users.Bio.Eq(""),
))                                                     // either
```

### One column against another

A shipment that weighs more than it was quoted for:

```go
db.Shipments.Query().Where(orm.OpPredicate[Shipment](
    ">", orm.ArgOf(Shipments.ActualGrams), orm.ArgOf(Shipments.QuotedGrams),
))
```

### A window of values

```go
db.Readings.Query().Where(Readings.Celsius.Between(-10, 45))
```

Two-sided and inclusive, which is what `BETWEEN` means in SQL — if you want it
exclusive at one end, say `Gte` and `Lt` instead and the reader can see which.

### Everything in a set, from a slice

```go
db.Flights.Query().Where(Flights.Origin.In("LHR", "CDG", "AMS"))
db.Flights.Query().Where(Flights.Origin.In(hubs...))
```

### An empty slice is not a bug

```go
// In() over nothing matches nothing, which is the SQL answer and rarely
// the one a caller expected. Decide it where the intent is.
if len(codes) == 0 {
    return nil, nil
}
db.Flights.Query().Where(Flights.Origin.In(codes...))
```

### Case-insensitive without a function on the column

```go
db.Artists.Query().Where(Artists.Name.ILike(input))
```

`ILike` with no wildcards is an equality that ignores case, and it stays
index-eligible on a `citext` column or one with a matching expression index —
which `lower(name) = lower($1)` does not, unless that exact index exists.

### Prefix search that an index can serve

```go
db.Artists.Query().Where(Artists.Name.Like(prefix + "%"))
```

Leading wildcards (`"%" + s`) cannot use a B-tree. If you need those, you want
[full-text search](/en/docs/fulltext/) or a trigram index, not `LIKE`.

### Three states from a nullable boolean

```go
db.Applications.Query().Where(Applications.Approved.Eq(true))   // approved
db.Applications.Query().Where(Applications.Approved.Eq(false))  // rejected
db.Applications.Query().Where(Applications.Approved.IsNull())   // undecided
```

### Nested and/or, kept readable

```go
db.Tickets.Query().Where(orm.And(
    Tickets.EventID.Eq(eventID),
    orm.Or(
        Tickets.Status.Eq("reserved"),
        orm.And(
            Tickets.Status.Eq("pending"),
            Tickets.HeldUntil.Gt(time.Now()),
        ),
    ),
))
```

### A filter built from a map

```go
q := db.Devices.Query()
for _, f := range []struct {
    want string
    eq   func(string) orm.Predicate[Device]
}{
    {model, Devices.Model.Eq},
    {region, Devices.Region.Eq},
} {
    if f.want != "" {
        q = q.Where(f.eq(f.want))
    }
}
```

### Excluding by a subquery

```go
db.Users.Query().Where(orm.NotInSub(
    orm.Of(Users.ID),
    orm.Compose(pool, blockedIDs).From(Blocks.Source()),
))
```

## Sorting

### Two keys, opposite directions

```go
db.Leaderboard.Query().OrderBy(
    Leaderboard.Score.Desc(),
    Leaderboard.AchievedAt.Asc(),
)
```

The tiebreak is not decoration. Without it the order of equal scores is
whatever the plan produced, and it changes between runs.

### NULLs where you want them

```go
db.Tasks.Query().OrderBy(Tasks.DueAt.Asc())
```

PostgreSQL sorts NULLs last for `ASC` and first for `DESC`. If undated tasks
belong at the end of a `DESC` list, sort by a coalesced expression instead:

```go
due := orm.CoalesceNull(orm.Of(Tasks.DueAt), orm.Val(farFuture))

orm.Select(db.Tasks, shape).OrderBy(due.Desc())
```

### Ordering by something you also selected

```go
distance := postgis.OfGeog(Stops.Spot).Distance(postgis.GeogValue[Stop](here))

orm.Select(db.Stops, nearest).OrderBy(distance.Asc()).Limit(10)
```

### Ordering by an aggregate

```go
orm.Select(db.Orders, byCustomer).
    GroupBy(Orders.CustomerID).
    OrderBy(orm.Count[Order]().Desc()).
    Limit(25)
```

### A stable order for exports

```go
db.Invoices.Query().OrderBy(Invoices.IssuedAt.Asc(), Invoices.ID.Asc())
```

Any export compared between two runs needs a total order. The primary key at
the end is the cheapest way to guarantee one.

### Random sample

```go
db.Photos.Query().
    OrderBy(orm.Fn[Photo, float64]("random").Asc()).
    Limit(10)
```

Fine for a hundred thousand rows and wrong for a hundred million — it sorts the
whole table. At that size, sample by a key range instead.

## Paging

### Offset paging

```go
db.Users.Query().OrderBy(Users.ID.Asc()).Limit(20).Offset(page * 20)
```

### Keyset paging

Correct on a moving table, and it stays fast at page 5000:

```go
q := db.Users.Query().OrderBy(Users.CreatedAt.Desc(), Users.ID.Desc()).Limit(20)
if cursor != nil {
    q = q.Where(orm.Or(
        Users.CreatedAt.Lt(cursor.At),
        orm.And(Users.CreatedAt.Eq(cursor.At), Users.ID.Lt(cursor.ID)),
    ))
}
```

### Keyset paging on a single unique key

When the sort column is already unique, the tuple comparison collapses:

```go
q := db.Events.Query().OrderBy(Events.Seq.Asc()).Limit(500)
if after > 0 {
    q = q.Where(Events.Seq.Gt(after))
}
```

### Total plus page, one round trip each

```go
total, err := db.Users.Query().Where(cond).Count(ctx)
page,  err := db.Users.Query().Where(cond).Limit(20).All(ctx)
```

### Is there a next page

Cheaper than a count, and usually the only thing the UI needs:

```go
rows, err := db.Users.Query().OrderBy(Users.ID.Asc()).Limit(21).All(ctx)
hasNext := len(rows) > 20
if hasNext {
    rows = rows[:20]
}
```

### Streaming a whole table without holding it

```go
rows, err := db.Events.Query().OrderBy(Events.ID.Asc()).Rows(ctx)
if err != nil {
    return err
}
defer rows.Close()

for rows.Next() {
    e, err := rows.Value()
    if err != nil {
        return err
    }
    if err := sink(e); err != nil {
        return err
    }
}
return rows.Err()
```

## Aggregation

### Count per group

```go
type ByStatus struct {
    Status string
    N      int64
}

var byStatus = orm.Project2(
    Orders.Status, orm.Count[Order](),
    func(s string, n int64) ByStatus { return ByStatus{s, n} },
)

rows, _ := orm.Select(db.Orders, byStatus).
    GroupBy(Orders.Status).
    OrderBy(Orders.Status.Asc()).
    All(ctx)
```

### Only the busy groups

```go
orm.Select(db.Orders, byStatus).
    GroupBy(Orders.Status).
    Having(orm.Count[Order]().Gt(100))
```

### Aggregates over no rows

```go
var maxShape = orm.Project1(orm.Max(Orders.Total), func(v *int64) *int64 { return v })
// nil when there are no rows — max over nothing is NULL, and the type says so
```

### Count of a nullable column is not count(*)

```go
var coverage = orm.Project2(
    orm.Count[User](),          // every row
    orm.CountOf(Users.Bio),     // rows whose bio is not NULL
    func(all, withBio int64) Coverage { return Coverage{all, withBio} },
)
```

### Distinct count

```go
var uniqueVisitors = orm.Project1(
    orm.CountOf(Visits.SessionID).Distinct(),
    func(n int64) int64 { return n },
)
```

### Several aggregates in one pass

The whole point of `GROUP BY` — one scan, five numbers:

```go
type Daily struct {
    Day     time.Time
    Orders  int64
    Revenue *int64
    Largest *int64
    Average *float64
}

day := orm.DateTrunc("day", orm.Of(Orders.PlacedAt))

var daily = orm.Project5(
    orm.Named("day", day),
    orm.Count[Order](),
    orm.SumInt32(Orders.TotalCents),
    orm.Max(Orders.TotalCents),
    orm.AvgInt32(Orders.TotalCents),
    func(d time.Time, n int64, sum, max *int64, avg *float64) Daily {
        return Daily{Day: d, Orders: n, Revenue: sum, Largest: max, Average: avg}
    },
)
```

### Conditional aggregates

Counting two things at once, without two queries:

```go
paid := orm.Count[Order]().Filter(Orders.Status.Eq("paid"))
refunded := orm.Count[Order]().Filter(Orders.Status.Eq("refunded"))

var split = orm.Project3(
    Orders.CustomerID, paid, refunded,
    func(id int64, p, r int64) Split { return Split{id, p, r} },
)
```

`FILTER` is the clause for this. `sum(case when … then 1 else 0 end)` is the
same answer written less clearly.

### Grouping by a derived value

```go
month := orm.DateTrunc("month", orm.Of(Subscriptions.StartedAt))

orm.Select(db.Subscriptions, monthly).
    GroupBy(month).
    OrderBy(month.Asc())
```

Group by the expression, not by an alias — the alias does not exist yet where
`GROUP BY` is evaluated.

### Two grouping keys

```go
orm.Select(db.Sales, byRegionAndQuarter).
    GroupBy(Sales.Region, quarter).
    OrderBy(Sales.Region.Asc(), quarter.Asc())
```

### Averages that stay honest

```go
orm.AvgInt32(Ratings.Stars)   // *float64 — NULL over no rows
orm.SumInt32(Ratings.Stars)   // *int64   — NULL over no rows
orm.Count[Rating]()           // int64    — zero over no rows
```

The pointer is not caution. `avg` over an empty group is NULL, and a `float64`
has no value that means "there was nothing to average".

### Percentage of a total

```go
var share = orm.Project2(
    Sales.Region,
    orm.Named("pct", orm.Op(
        orm.Op(orm.SumInt32(Sales.Cents), "*", orm.Val(int64(100))),
        "/",
        orm.Fn[Sale, int64]("sum", orm.Of(Sales.Cents)).Over(orm.Window[Sale]()),
    )),
    func(region string, pct *int64) Share { return Share{region, pct} },
)
```

### The busiest hour of each day

```go
hour := orm.DateTrunc("hour", orm.Of(Rides.StartedAt))
day := orm.DateTrunc("day", orm.Of(Rides.StartedAt))

ranked := orm.RowNumber[Ride]().Over(
    orm.Window[Ride]().PartitionBy(day).OrderBy(orm.Count[Ride]().Desc()),
)
```

### Distinct on: one row per group, cheaply

```go
orm.Select(db.Prices, latest).
    DistinctOn(Prices.SKU).
    OrderBy(Prices.SKU.Asc(), Prices.ObservedAt.Desc())
```

The `ORDER BY` must start with the `DISTINCT ON` columns — that is what decides
which row of each group survives. Here: the newest price per SKU.

## Window functions

### Numbering rows within a group

```go
rank := orm.RowNumber[Result]().Over(
    orm.Window[Result]().
        PartitionBy(orm.Of(Results.HeatID)).
        OrderBy(orm.Of(Results.TimeMillis).Asc()),
)
```

### Rank, dense rank, and the difference

```go
w := orm.Window[Score]().OrderBy(orm.Of(Scores.Points).Desc())

orm.Rank[Score]().Over(w)       // 1, 2, 2, 4  — gaps after ties
orm.DenseRank[Score]().Over(w)  // 1, 2, 2, 3  — no gaps
orm.RowNumber[Score]().Over(w)  // 1, 2, 3, 4  — arbitrary among ties
```

### Running total

```go
w := orm.Window[Entry]().
    OrderBy(orm.Of(Entries.At).Asc()).
    Rows(orm.UnboundedPreceding(), orm.CurrentRow())

running := orm.Fn[Entry, int64]("sum", orm.Of(Entries.Cents)).Over(w)
```

### Change since the previous row

```go
prev := orm.Lag(orm.Of(Readings.Celsius)).Over(
    orm.Window[Reading]().
        PartitionBy(orm.Of(Readings.SensorID)).
        OrderBy(orm.Of(Readings.At).Asc()),
)
```

### A moving average

```go
w := orm.Window[Tick]().
    OrderBy(orm.Of(Ticks.At).Asc()).
    Rows(orm.Preceding(6), orm.CurrentRow())

sevenDay := orm.Fn[Tick, float64]("avg", orm.Of(Ticks.Price)).Over(w)
```

### First and last in a partition

```go
w := orm.Window[Event]().
    PartitionBy(orm.Of(Events.SessionID)).
    OrderBy(orm.Of(Events.At).Asc()).
    Rows(orm.UnboundedPreceding(), orm.UnboundedFollowing())

orm.FirstValue(orm.Of(Events.Page)).Over(w)  // the landing page
orm.LastValue(orm.Of(Events.Page)).Over(w)   // the exit page
```

`LastValue` needs the explicit frame. With the default frame it returns the
current row, which is the single most common window-function surprise.

### Quartiles

```go
orm.Ntile[Customer](4).Over(
    orm.Window[Customer]().OrderBy(orm.Of(Customers.LifetimeCents).Desc()),
)
```

### One window reused

```go
w := orm.Window[Order]().
    PartitionBy(orm.Of(Orders.CustomerID)).
    OrderBy(orm.Of(Orders.PlacedAt).Asc())

seq := orm.RowNumber[Order]().Over(w)
prevAt := orm.Lag(orm.Of(Orders.PlacedAt)).Over(w)
firstAt := orm.FirstValue(orm.Of(Orders.PlacedAt)).Over(w)
```

## Joins and composition

### An inner join with a projection

```go
type Line struct {
    Order   int64
    Product string
    Qty     int32
}

var lines = orm.Project3(
    orm.Of(Items.OrderID), orm.Of(Products.Name), orm.Of(Items.Qty),
    func(o int64, p string, q int32) Line { return Line{o, p, q} },
)

rows, err := orm.Compose(pool, lines).
    From(Items.Source()).
    Join(Products.Source(), orm.Of(Items.ProductID).EqCol(orm.Of(Products.ID))).
    All(ctx)
```

### A left join, and the nullability it forces

```go
var withLast = orm.Project2(
    orm.Of(Users.Email), orm.Opt(Orders.PlacedAt),
    func(email string, last *time.Time) Row { return Row{email, last} },
)

orm.Compose(pool, withLast).
    From(Users.Source()).
    LeftJoin(Orders.Source(), orm.Of(Users.ID).EqCol(orm.Of(Orders.UserID)))
```

`orm.Opt` is not optional here. The outer join can produce NULL for every
column of the right side, and a `time.Time` destination cannot hold that.

### Joining three tables

```go
orm.Compose(pool, shape).
    From(Orders.Source()).
    Join(Customers.Source(), orm.Of(Orders.CustomerID).EqCol(orm.Of(Customers.ID))).
    Join(Regions.Source(), orm.Of(Customers.RegionID).EqCol(orm.Of(Regions.ID)))
```

### A self-join through an alias

Employees and their managers, from one table:

```go
mgr := Employees.As("mgr")

orm.Compose(pool, pairs).
    From(Employees.Source()).
    LeftJoin(mgr.Source(), orm.Of(Employees.ManagerID).EqCol(orm.Of(mgr.ID)))
```

The alias is a second occurrence of the same table, and the descriptors it
carries are bound to it — so `mgr.ID` cannot accidentally mean `Employees.ID`.

### A lateral join for the top N per row

```go
recent := orm.Compose(pool, orderShape).
    From(Orders.Source()).
    Where(orm.Of(Orders.CustomerID).EqCol(orm.Of(Customers.ID))).
    OrderBy(orm.Of(Orders.PlacedAt).Desc()).
    Limit(3)

orm.Compose(pool, shape).
    From(Customers.Source()).
    LeftJoinLateral(recent.As("recent"))
```

### A CTE, used twice

```go
active := orm.CTE("active", orm.Compose(pool, userShape).
    From(Users.Source()).
    Where(orm.Of(Users.Active).Eq(true)))

orm.Compose(pool, shape).
    With(active).
    From(active.Source()).
    Join(Orders.Source(), orm.Of(Orders.UserID).EqCol(orm.Of(active.ID)))
```

### UNION ALL of two shapes

```go
orm.UnionAll(
    orm.Compose(pool, feed).From(Posts.Source()),
    orm.Compose(pool, feed).From(Comments.Source()),
).OrderBy(orm.Of(feedAt).Desc()).Limit(50)
```

Both branches must produce the same shape — same column count, same types, same
nullability. That is checked when you build it, not when PostgreSQL runs it.

### An anti-join, two ways

```go
// Correlated NOT EXISTS — usually the plan you want.
orm.Compose(pool, shape).From(Users.Source()).Where(
    orm.NotExists(orm.Compose(pool, one).
        From(Orders.Source()).
        Where(orm.Of(Orders.UserID).EqCol(orm.Of(Users.ID)))),
)

// Left join and test for NULL — the same rows, a different plan.
orm.Compose(pool, shape).
    From(Users.Source()).
    LeftJoin(Orders.Source(), orm.Of(Users.ID).EqCol(orm.Of(Orders.UserID))).
    Where(orm.Opt(Orders.ID).IsNull())
```

### A scalar subquery in the select list

```go
orderCount := orm.Scalar(orm.Compose(pool, countShape).
    From(Orders.Source()).
    Where(orm.Of(Orders.UserID).EqCol(orm.Of(Users.ID))))

var withCount = orm.Project2(
    orm.Of(Users.Email), orm.Named("orders", orderCount),
    func(email string, n *int64) Row { return Row{email, n} },
)
```

### Cross join for a dense calendar

Every day crossed with every product, so a day with no sales still gets a row:

```go
orm.Compose(pool, shape).
    From(days.Source()).
    CrossJoin(Products.Source()).
    LeftJoin(Sales.Source(), orm.And(
        orm.Of(Sales.Day).EqCol(orm.Of(days.Day)),
        orm.Of(Sales.ProductID).EqCol(orm.Of(Products.ID)),
    ))
```

## Relations

### The five most recent per parent

One statement, not one per user:

```go
db.Users.Query().
    With(Users.Orders.OrderBy(Orders.Placed.Desc()).Limit(5)).
    All(ctx)
```

### Parents that have a child

```go
db.Users.Query().Where(Users.Orders.Any(Orders.Status.Eq("paid")))
```

### Parents that have none

```go
db.Users.Query().Where(Users.Orders.None())
```

### Deep loading

```go
db.Users.Query().
    With(Users.Orders.With(Orders.Items.With(Items.Product))).
    All(ctx)
// four statements regardless of row counts
```

### Loading a filtered branch

```go
db.Users.Query().
    With(Users.Orders.Where(Orders.Status.Eq("paid"))).
    All(ctx)
```

The filter applies to the child load, not to the parents. Users with no paid
orders still come back, with an empty slice.

### Filtering parents by a child's field

```go
db.Albums.Query().Where(Albums.Tracks.Any(Tracks.DurationMs.Gt(600_000)))
```

### Parents where every child qualifies

Expressed as "no child fails", which is what SQL can actually check:

```go
db.Orders.Query().Where(orm.Not(Orders.Items.Any(Items.InStock.Eq(false))))
```

### A relation and an aggregate side by side

```go
db.Playlists.Query().
    With(Playlists.Tracks.Limit(3)).       // a preview of the contents
    All(ctx)

// the true size, separately, because a limited load cannot tell you it
orm.Select(db.Tracks, byPlaylist).GroupBy(Tracks.PlaylistID)
```

### Many-to-many through a join table

```go
db.Students.Query().
    With(Students.Courses).
    All(ctx)
```

### The unloaded case is visible

```go
u, _ := db.Users.Query().One(ctx)   // no With
// u.Orders is nil, and nil means "not loaded", not "none exist".
// Nothing fetches it behind the field access — a loop cannot become N queries.
```

## Writing

### Insert one, keep what the database decided

```go
u, err := db.Users.Insert(ctx, User{Email: "ada@example.com", Name: "Ada"})
if err != nil {
    return err
}
// the returned value carries what the database decided: u.ID, u.CreatedAt
```

### Insert many in one statement

```go
saved, err := db.Tags.InsertMany(ctx, tags)
if err != nil {
    return err
}
```

### A default instead of a zero value

```go
db.Users.Insert(ctx, u, orm.Default(Users.Role))
```

`Role: ""` means the empty string, because a Go zero value is a value. Asking
for the column default is a separate thing, so it is a separate call.

### Update by primary key

```go
n, err := db.Users.Update().
    Set(Users.Name.Set("Ada Lovelace")).
    Where(Users.ID.Eq(id)).
    Exec(ctx)
```

### Increment without reading first

```go
db.Counters.Update().
    Set(Counters.Hits.SetExpr(Counters.Hits.Add(1))).
    Where(Counters.Key.Eq(key)).
    Exec(ctx)
```

### Set a column from another column

```go
db.Invoices.Update().
    Set(Invoices.BalanceCents.SetExpr(Invoices.TotalCents.SubCol(Invoices.PaidCents))).
    Where(Invoices.ID.Eq(id)).
    Exec(ctx)
```

### Clear a nullable column

```go
db.Users.Update().
    Set(Users.DeactivatedAt.SetNull()).
    Where(Users.ID.Eq(id)).
    Exec(ctx)
```

### Update and read the result back

```go
type Moved struct {
    ID    int64
    State string
}

var moved = orm.Project2(
    Jobs.ID, Jobs.Status,
    func(id int64, s string) Moved { return Moved{id, s} },
)

rows, err := orm.UpdateReturning(
    db.Jobs.Update().Set(Jobs.Status.Set("running")).Where(Jobs.Status.Eq("pending")),
    moved,
).All(ctx)
```

One statement. The alternative — update, then select what you just updated — is
two statements and a race between them.

### Delete and keep the rows

```go
gone, err := orm.DeleteReturningEntity(
    db.Sessions.Delete().Where(Sessions.ExpiresAt.Lt(time.Now())),
).All(ctx)
```

### Bulk load

```go
n, err := db.Events.CopyFrom(ctx, batch)
```

`COPY` rather than a multi-row `INSERT`. It is dramatically faster and it does
not support `ON CONFLICT` — load into a staging table and merge if you need both.

### Bulk load from a stream

```go
n, err := db.Events.CopyFromSeq(ctx, func(yield func(Event) bool) {
    for scanner.Scan() {
        e, err := parse(scanner.Text())
        if err != nil {
            return
        }
        if !yield(e) {
            return
        }
    }
})
```

Nothing holds the whole file. The rows go to the server as they are parsed.

### Delete in bounded batches

A ten-million-row delete as a sequence of small transactions, so nothing holds
a lock for an hour:

```go
for {
    n, err := db.Events.Delete().
        Where(Events.ID.In(nextIDs...)).
        Exec(ctx)
    if err != nil {
        return err
    }
    if n == 0 {
        return nil
    }
}
```

### Truncate, on purpose

```go
if err := ormtest.TruncateWith(ctx, pool,
    []ormtest.TruncateOption{ormtest.RestartIdentity()},
    Staging,
); err != nil {
    return err
}
```

Not a `DELETE` with no `WHERE`. It is a different statement with different
locking and no per-row work, and it is spelled differently so it cannot be
reached by forgetting a clause.

## Upserts and conflicts

### Upsert on a natural key

```go
db.Users.Insert(ctx, user,
    // Take the new row's values for these columns.
    orm.OnConflict(Users.Email).DoUpdate(Users.Name, Users.UpdatedAt),
)
```

### Insert if absent, ignore if present

```go
db.Tags.Insert(ctx, tag, orm.OnConflict(Tags.Slug).DoNothing())
```

### Upsert that computes the new value

Last-write-wins is wrong for a counter; this adds instead:

```go
db.Counters.Insert(ctx, c,
    orm.OnConflict(Counters.Key).DoUpdateSet(
        Counters.Hits.SetExpr(Counters.Hits.AddCol(orm.Excluded(Counters.Hits))),
    ),
)
```

`orm.Excluded` is the row that was proposed and rejected — the `EXCLUDED`
pseudo-table, named the same way it is in SQL.

### Only overwrite if the incoming row is newer

```go
db.Prices.Insert(ctx, p,
    orm.OnConflict(Prices.SKU).
        DoUpdate(Prices.Cents, Prices.ObservedAt).
        Where(Prices.ObservedAt.Lt(orm.Excluded(Prices.ObservedAt))),
)
```

### Upsert a whole batch

```go
db.Inventory.InsertMany(ctx, rows,
    orm.OnConflict(Inventory.SKU, Inventory.WarehouseID).
        DoUpdate(Inventory.OnHand),
)
```

The conflict target is the unique constraint's columns, in any order — but
there must actually be one, or PostgreSQL has nothing to detect a conflict on.

## JSON and arrays

These are free functions producing a `Predicate[Composed]`, so they go in a
composed query. `orm.Opt` lifts a non-nullable column:

```go
meta := orm.Opt(Users.Meta)
tags := orm.Opt(Users.Tags)

orm.Compose(pool, shape).From(Users.Source()).Where(
    orm.JSONHasKey(meta, "plan"),
)

orm.Compose(pool, shape).From(Users.Source()).Where(
    orm.JSONContains(meta, orm.Val(map[string]any{"plan": "pro"})),
)

// the text at a path, cast to something comparable
tier := orm.CastNull(orm.JSONPathText(meta, "billing", "tier"), orm.Text)

orm.Compose(pool, shape).From(Users.Source()).Where(
    orm.ArrayContains(tags, orm.Val([]string{"go", "sql"})),
    orm.ArrayOverlaps(tags, orm.Val([]string{"go"})),
)
```

### Any of these keys, all of these keys

```go
orm.JSONHasAnyKeys(meta, "plan", "trial")
orm.JSONHasAllKeys(meta, "plan", "seats")
```

### Reading a nested value out

```go
city := orm.JSONPathText(orm.Opt(Profiles.Data), "address", "city")

var byCity = orm.Project2(
    orm.Of(Profiles.UserID), orm.Named("city", city),
    func(id int64, city *string) Row { return Row{id, city} },
)
```

### An element by index

```go
first := orm.JSONIndexText(orm.Opt(Orders.Lines), 0)
```

### Writing into a JSON document

```go
db.Profiles.Update().
    Set(Profiles.Data.SetExpr(orm.JSONSet(
        orm.Opt(Profiles.Data), orm.Val("verified"), orm.Val(true),
    ))).
    Where(Profiles.UserID.Eq(id)).
    Exec(ctx)
```

### Dropping nulls before storing

```go
orm.JSONStripNulls(orm.Opt(Profiles.Data))
```

### Array length

```go
tagCount := orm.Fn[Post, int32]("array_length", orm.ArgOf(Posts.Tags), orm.ArgValue(1))

orm.Select(db.Posts, shape).Where(tagCount.Gt(3))
```

### An array that contains all of several values

```go
orm.ArrayContains(orm.Opt(Posts.Tags), orm.Val([]string{"go", "postgres"}))
```

Contains means "is a superset of". For "has at least one of", use
`ArrayOverlaps` — they are different questions and the operators differ too.

### An array contained by an allow-list

```go
orm.ArrayContainedBy(orm.Opt(Roles.Granted), orm.Val(allowed))
```

## Full text search

```go
q := orm.PlainToTSQuery(orm.English, input)

type Hit struct {
    ID    int64
    Title string
    Rank  float32
}

var hits = orm.Project3(
    Docs.ID, Docs.Title, orm.TSRank(Docs.Search, q),
    func(id int64, title string, rank float32) Hit { return Hit{id, title, rank} },
)

orm.Select(db.Docs, hits).
    Where(orm.Matches(Docs.Search, q)).
    OrderBy(orm.TSRank(Docs.Search, q).Desc()).
    Limit(20).
    All(ctx)
```

### Accepting search-engine syntax from a user

```go
q := orm.WebSearchToTSQuery(orm.English, input)
```

`websearch_to_tsquery` accepts quoted phrases, `or`, and `-exclusion`, and never
raises a syntax error on nonsense — which is what you want from a text box.
`to_tsquery` does raise, so it is the wrong one to point at user input.

### A phrase, in order

```go
q := orm.PhraseToTSQuery(orm.English, "ada lovelace")
```

### Combining queries

```go
must := orm.PlainToTSQuery(orm.English, required)
nice := orm.PlainToTSQuery(orm.English, optional)

orm.AndTSQuery(must, orm.NotTSQuery(nice))
```

### Weighting the title above the body

```go
vec := orm.Concat2TSVector(
    orm.SetWeight(orm.ToTSVector(orm.English, orm.Of(Docs.Title)), "A"),
    orm.SetWeight(orm.ToTSVector(orm.English, orm.Of(Docs.Body)), "B"),
)
```

### Ranking that accounts for distance

```go
orm.TSRankCD(Docs.Search, q).Desc()
```

## Time, dates and ranges

```go
db.Bookings.Query().Where(Bookings.During.Overlaps(
    orm.ClosedOpen(from, to),
))

db.Events.Query().Where(Events.At.Between(dayStart, dayEnd))
```

### Truncating to a period

```go
month := orm.DateTrunc("month", orm.Of(Invoices.IssuedAt))
```

### Pulling a field out

```go
dow := orm.Extract("dow", orm.Of(Rides.StartedAt))
year := orm.Extract("year", orm.Of(Rides.StartedAt))
```

### Server time, not client time

```go
db.Sessions.Update().
    Set(Sessions.SeenAt.SetExpr(orm.Now[Session]())).
    Where(Sessions.ID.Eq(id)).
    Exec(ctx)
```

The database's clock is the one every row already agrees with. The application
server's may be seconds away from it, and in a cluster, away from itself.

### Adding an interval

```go
expires := orm.AddInterval(orm.Of(Tokens.IssuedAt), orm.IntervalOf(24*time.Hour))
```

### Anything expiring in the next hour

```go
db.Tokens.Query().Where(Tokens.ExpiresAt.Between(now, now.Add(time.Hour)))
```

### A range that contains a point

```go
db.Rates.Query().Where(Rates.Effective.Contains(when))
```

### Two bookings that would collide

```go
db.Bookings.Query().Where(orm.And(
    Bookings.RoomID.Eq(room),
    Bookings.During.Overlaps(orm.ClosedOpen(from, to)),
))
```

Overlap is the question, and a range type answers it in one operator. Written
as four comparisons on two columns it is the same query with more places to get
a boundary wrong.

### Bounds, inclusive and otherwise

```go
orm.ClosedOpen(from, to)   // [from, to)  — the usual one for time
orm.Closed(from, to)       // [from, to]
orm.Open(from, to)         // (from, to)
orm.OpenClosed(from, to)   // (from, to]
```

Half-open is the right default for time: two adjacent `[a, b)` ranges tile
without overlapping, and `[a, b]` ones do not.

### Empty and unbounded

```go
orm.RangeFrom(start)     // [start, ∞)
orm.RangeUntil(end)      // (-∞, end)
orm.EmptyRange[Booking]() // matches nothing, and is not the same as NULL
```

## Transactions and locking

### A transaction

```go
err := db.Tx(ctx, func(tx *domain.DB) error {
    if err := tx.Accounts.Update().
        Set(Accounts.Cents.SetExpr(Accounts.Cents.Sub(amount))).
        Where(Accounts.ID.Eq(from)).
        Exec(ctx); err != nil {
        return err
    }
    return tx.Accounts.Update().
        Set(Accounts.Cents.SetExpr(Accounts.Cents.Add(amount))).
        Where(Accounts.ID.Eq(to)).
        Exec(ctx)
})
```

Returning an error rolls back. There is no global transaction and nothing is
implicit — `tx` is a different handle from `db`, so a stray `db` call inside the
closure is visible in review.

### Claim work safely

The queue pattern, with two workers never taking the same row:

```go
err := db.Tx(ctx, func(tx *domain.DB) error {
    jobs, err := tx.Jobs.Query().
        Where(Jobs.Status.Eq("pending")).
        OrderBy(Jobs.Created.Asc()).
        Limit(10).
        Lock(orm.ForUpdateStrong, orm.SkipLocked()).
        All(ctx)
    if err != nil {
        return err
    }
    for _, j := range jobs {
        if _, err := tx.Jobs.Update().
            Set(Jobs.Status.Set("running")).
            Where(Jobs.ID.Eq(j.ID)).
            Exec(ctx); err != nil {
            return err
        }
    }
    return nil
})
```

### Fail rather than wait

```go
db.Accounts.Query().
    Where(Accounts.ID.Eq(id)).
    Lock(orm.ForUpdateStrong, orm.NoWait()).
    One(ctx)
```

### A read that must not block writers

```go
db.Reports.Query().Lock(orm.ForShare).All(ctx)
```

### An isolation level

```go
err := db.TxOptions(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx *domain.DB) error {
    return transfer(ctx, tx)
})
```

Serializable can fail with a serialization error that is safe to retry. That
retry belongs in your code, because only you know whether the work is idempotent.

## Escape hatches

### A fragment inside a built query

```go
db.Users.Query().Where(
    orm.Expr[User]("age(created_at) > interval ?", "1 year"),
)
```

### A whole statement, with the generated scanner kept

```go
users, err := orm.Raw[User](db.Users, `
    SELECT * FROM users
    WHERE ctid = ANY (?)
`, ctids).All(ctx)
```

Both take SQL text deliberately. Neither takes values formatted into it.

### Calling a function the ORM does not wrap

```go
soundex := orm.Fn[Person, string]("soundex", orm.ArgOf(People.Surname))
wanted := orm.Fn[Person, string]("soundex", orm.ArgValue(input))

orm.Select(db.People, shape).Where(soundex.EqCol(wanted))
```

### An operator the ORM does not wrap

```go
similar := orm.OpPredicate[Product]("%>", orm.ArgOf(Products.Name), orm.ArgValue(input))
```

### Seeing the SQL before running it

```go
sql, args, err := db.Users.Query().Where(Users.Active.Eq(true)).SQL()
```

### Reading the plan

```go
plan, err := db.Users.Query().Where(Users.Email.Eq(addr)).Explain(ctx)
```

`ExplainAnalyze` runs the statement. On a `SELECT` that is usually fine; on
anything that writes, it is not a preview — it does the work.
