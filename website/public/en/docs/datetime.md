# Dates and intervals

> Truncating, extracting, and an interval type that refuses to lie about months.

Source: https://ormgo.vercel.app/en/docs/datetime/
Symbols: https://ormgo.vercel.app/api/orm.txt — the generated list of every exported name.

---
## Interval is not a Duration

`time.Duration` is a count of nanoseconds. A PostgreSQL `interval` is three
independent parts — months, days and microseconds — and it keeps them apart
because they are not convertible:

- a month is 28 to 31 days
- a day is 23, 24 or 25 hours across a daylight-saving boundary

```go
iv := orm.IntervalOf(months, days, micros)
iv := orm.IntervalFromDuration(90 * time.Minute) // no months, no days
```

Converting back only works when there is nothing calendar-shaped in it:

```go
d, err := iv.Duration()
if errors.Is(err, orm.ErrCalendarInterval) {
    // months or days are present; what they mean depends on when they start,
    // and the library will not pick 30 days on your behalf
}
```

That error is the whole design. A library that silently returned 720 hours for
one month would be right most of the time and wrong at every month boundary.

## Arithmetic

```go
orm.AddInterval(Events.At, orm.Val(iv))   // timestamp + interval
orm.SubInterval(Events.At, orm.Val(iv))
orm.IntervalPlus(a, b)                    // interval + interval
orm.IntervalMinus(a, b)
orm.IntervalTimes(a, 3)                   // interval * n
```

Each has a `…Null` form for nullable inputs, because arithmetic with NULL is
NULL.

## Truncating

```go
orm.DateTrunc(orm.Month, Events.At)   // date_trunc('month', at)
orm.DateTrunc(orm.Day, Events.At)
orm.DateTrunc(orm.Hour, Events.At)
```

The classic use is grouping a time series into buckets:

```go
bucket := orm.DateTrunc(orm.Day, Events.At)

var perDay = orm.Project2(
    bucket, orm.Count[Event](),
    func(day time.Time, n int64) Bucket { return Bucket{day, n} },
)

orm.Select(db.Events, perDay).
    Where(Events.At.Gte(since)).
    GroupBy(bucket).
    OrderBy(bucket.Asc()).
    All(ctx)
```

Group by the **same expression** you selected. Two `DateTrunc` calls with the
same arguments render the same SQL, so PostgreSQL matches them — but binding it
to a variable says so, and reads better.

## Extracting

```go
orm.Extract(orm.Year, Events.At, orm.Integer)         // -> int32
orm.Extract(orm.DayOfWeek, Events.At, orm.Integer)    // 0 = Sunday
orm.Extract(orm.EpochSecond, Events.At, orm.BigInt)   // -> int64
```

The third argument is the type you want back, as a `PGType` value. PostgreSQL's
`extract` returns `numeric`, so something has to say what to cast it to, and
saying it here means the Go type is decided rather than asserted.

The fields:

```go
orm.Year   orm.Quarter  orm.Month  orm.Week  orm.Day
orm.Hour   orm.Minute   orm.Second
orm.DayOfWeek  orm.DayOfYear  orm.EpochSecond
```

## Comparing

Timestamps are ordered columns, so the ordinary predicates apply:

```go
db.Events.Query().Where(Events.At.Between(dayStart, dayEnd))
db.Events.Query().Where(Events.At.Gte(cutoff))
db.Events.Query().OrderBy(Events.At.Desc())
```

For "within the last N", compute the boundary in Go rather than in SQL when you
can — a bind parameter is a better plan input than an expression the planner has
to evaluate per row.

## Worked examples

### A daily signup chart

```go
day := orm.DateTrunc(orm.Day, Accounts.CreatedAt)

var perDay = orm.Project2(
    day, orm.Count[Account](),
    func(d time.Time, n int64) Point { return Point{d, n} },
)

rows, err := orm.Select(db.Accounts, perDay).
    Where(Accounts.CreatedAt.Gte(since)).
    GroupBy(day).
    OrderBy(day.Asc()).
    All(ctx)
```

Monthly is the same query with one word changed, which is the point of naming
the bucket.

### Opening hours

```go
hour := orm.Extract(orm.Hour, Visits.At, orm.Integer)
dow  := orm.Extract(orm.DayOfWeek, Visits.At, orm.Integer)

var heat = orm.Project3(
    dow, hour, orm.Count[Visit](),
    func(d, h int32, n int64) Cell { return Cell{d, h, n} },
)

orm.Select(db.Visits, heat).GroupBy(dow, hour).All(ctx)
```

### A trial that expires

```go
// Trials ending in the next three days.
soon := time.Now().Add(72 * time.Hour)
db.Trials.Query().Where(Trials.EndsAt.Between(time.Now(), soon))

// Extending one, in SQL, without reading it first.
db.Trials.Update().
    Set(Trials.EndsAt.SetExpr(orm.AddInterval(Trials.EndsAt, orm.Val(orm.IntervalOf(0, 14, 0))))).
    Where(Trials.ID.Eq(id)).
    Exec(ctx)
```

`IntervalOf(0, 14, 0)` is fourteen **days**, not 336 hours. Across a
daylight-saving boundary those are different instants, and the interval keeps
the distinction that a `Duration` would throw away.
