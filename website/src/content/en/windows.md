---
title: Window functions
description: Ranking, offsets and running totals — computed per row, without collapsing them.
---

## What they do

An aggregate collapses rows: `count(*)` over ten rows returns one. A window
function computes across rows and **keeps every one of them** — each row gets its
own answer, calculated from the rows around it.

```go
rn := orm.RowNumber().Over(orm.Window().
    PartitionBy(orm.Of(Posts.AuthorID)).
    OrderBy(orm.Of(Posts.CreatedAt).Desc()))
```

```sql
row_number() OVER (PARTITION BY author_id ORDER BY created_at DESC)
```

Read the window as: **restart the numbering for each author**, and **within an
author, order by newest first**.

## The two halves

A window function is always a function plus a window:

```go
orm.RowNumber().Over(orm.Window()...)
//  └ the function        └ the window it looks through
```

`orm.Window()` builds the window:

| Method | Adds |
| --- | --- |
| `PartitionBy(...)` | `PARTITION BY` — restart for each group |
| `OrderBy(...)` | `ORDER BY` — the order within a partition |
| `Rows(start, end)` | `ROWS` frame |
| `Range(start, end)` | `RANGE` frame |
| `Groups(start, end)` | `GROUPS` frame |

Any of them may be omitted. `Over(orm.Window())` with nothing set is one window
over the whole result.

## Using one

A window function is an expression, so it goes in a projection like any other:

```go
type Ranked struct {
    Title string
    N     int64
}

rn := orm.RowNumber().Over(orm.Window().
    PartitionBy(orm.Of(Posts.AuthorID)).
    OrderBy(orm.Of(Posts.CreatedAt).Desc()))

shape := orm.Project2(
    orm.Of(Posts.Title), rn,
    func(title string, n int64) Ranked { return Ranked{title, n} },
)

rows, err := orm.Compose(pool, shape).From(Posts.Source()).All(ctx)
```

## The functions

### Ranking

```go
orm.RowNumber()    // 1, 2, 3, 4        -> int64
orm.Rank()         // 1, 2, 2, 4        -> int64  (ties share, then skip)
orm.DenseRank()    // 1, 2, 2, 3        -> int64  (ties share, no gap)
orm.PercentRank()  // 0.0 … 1.0         -> float64
orm.CumeDist()     // cumulative        -> float64
orm.Ntile(4)       // quartile bucket   -> int32
```

`Rank` and `DenseRank` differ only in what happens after a tie, which is the
thing people get wrong: `Rank` leaves a hole, `DenseRank` does not.

### Reaching other rows

```go
orm.Lag(Posts.Score)         // the previous row's value
orm.LagN(Posts.Score, 3)     // three rows back
orm.Lead(Posts.Score)        // the next row's value
orm.LeadN(Posts.Score, 3)
orm.FirstValue(Posts.Score)  // first in the frame
orm.LastValue(Posts.Score)   // last in the frame
orm.NthValue(Posts.Score, 2) // the second
```

All of these return the **nullable** form of the column's type. There is no
previous row for the first row, and no next row for the last — so `Lag` over a
`NOT NULL` column is still `*T`, and the type says so rather than letting a NULL
arrive at a destination that cannot hold it.

### Aggregates as windows

Any aggregate becomes a window function with `Over`:

```go
running := orm.SumInt64[Order, int64](Orders.Total).Over(orm.Window().
    OrderBy(orm.Of(Orders.Placed).Asc()).
    Rows(orm.UnboundedPreceding(), orm.CurrentRow()))
```

```sql
sum(total) OVER (ORDER BY placed ASC ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW)
```

That is a running total: every row sees itself and everything before it.

## Frames

A frame narrows which rows of the partition the function sees. The bounds:

```go
orm.UnboundedPreceding()  // the start of the partition
orm.Preceding(3)          // three rows back
orm.CurrentRow()
orm.Following(3)
orm.UnboundedFollowing()  // the end of the partition
```

```go
// a trailing 7-row average
orm.AvgInt64[Order, float64](Orders.Total).Over(orm.Window().
    OrderBy(orm.Of(Orders.Placed).Asc()).
    Rows(orm.Preceding(6), orm.CurrentRow()))
```

`Rows` counts rows. `Range` counts by value, so peers with equal `ORDER BY`
values are included together. `Groups` counts peer groups. They are different
answers on tied data, which is why all three exist rather than one.

## Where a window function cannot go

Not in `WHERE`, and not in `HAVING`. PostgreSQL evaluates windows after those
clauses, so the value does not exist yet.

To filter on one, compute it in a derived table and filter outside — which is
the Top-N recipe:

```go
rank := orm.Named("rn", orm.RowNumber().Over(orm.Window().
    PartitionBy(orm.Of(Orders.UserID)).
    OrderBy(orm.Of(Orders.Placed).Desc())))

ranked := orm.Sub("ranked", orm.Rows(
    orm.Named("id", orm.Of(Orders.ID)),
    rank,
).From(Orders.Source()))

rows, err := orm.Compose(pool, shape).
    From(ranked).
    Where(orm.Ref(ranked, rank).Lte(3)).   // the three most recent per user
    All(ctx)
```

See [Hard queries](/en/docs/cookbook/insane/) for the whole of that one.

## Worked examples

### A leaderboard with ties handled

```go
w := orm.Window().OrderBy(orm.Of(Scores.Points).Desc())

var board = orm.Project3(
    orm.Of(Scores.Player),
    orm.Rank().Over(w),        // 1, 2, 2, 4 — a tie leaves a hole
    orm.DenseRank().Over(w),   // 1, 2, 2, 3 — a tie does not
    func(p string, r, d int64) Row { return Row{p, r, d} },
)

orm.Compose(pool, board).From(Scores.Source()).All(ctx)
```

Which one is right depends on whether "third place" should exist when two people
tie for second. That is a product decision, and the two functions let you make it.

### Change since the previous reading

```go
w := orm.Window().
    PartitionBy(orm.Of(Meters.MeterID)).
    OrderBy(orm.Of(Meters.ReadAt).Asc())

previous := orm.Lag(Meters.Value).Over(w)

var deltas = orm.Project3(
    orm.Of(Meters.MeterID), orm.Of(Meters.Value), previous,
    func(id int64, now int32, before *int32) Delta { return Delta{id, now, before} },
)
```

`before` is a pointer because the first reading of each meter has nothing behind
it. Partitioning restarts that at every meter.

### A running balance

```go
running := orm.SumInt32[orm.Composed](orm.Of(Entries.AmountCents)).
    Over(orm.Window().
        PartitionBy(orm.Of(Entries.AccountID)).
        OrderBy(orm.Of(Entries.PostedAt).Asc()).
        Rows(orm.UnboundedPreceding(), orm.CurrentRow()))
```

### A seven-day moving average

```go
avg := orm.AvgInt64[orm.Composed, float64](orm.Of(Daily.Total)).
    Over(orm.Window().
        OrderBy(orm.Of(Daily.Day).Asc()).
        Rows(orm.Preceding(6), orm.CurrentRow()))
```

`Rows(6 preceding, current)` is seven rows including this one. Using `Range`
instead would group days with equal values together, which is not what a moving
average means.
