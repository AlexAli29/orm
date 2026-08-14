---
title: Ranges and multiranges
description: A span of values, with its bounds kept — not two columns pretending to be one.
---

## Why the type exists

A pair of endpoints cannot say whether a bound is inclusive, exclusive or
unbounded. `[1,10)` and `(1,10]` contain different numbers, and two `int`
columns called `lo` and `hi` have nowhere to record which you meant.

`Range[T]` carries the whole model: two values and two bound kinds, plus the
empty range, which is not the same as a range of width zero.

## Building one

```go
orm.Closed(1, 10)        // [1,10]  both ends included
orm.ClosedOpen(1, 10)    // [1,10)  the usual one for dates and times
orm.OpenClosed(1, 10)    // (1,10]
orm.RangeFrom(t)         // [t,)    no upper bound
orm.RangeUntil(t)        // (,t)    no lower bound
orm.UnboundedRange[int]() // (,)
orm.EmptyRange[int]()    // empty
```

`NewRange` is the explicit form when the bounds are computed:

```go
orm.NewRange(lo, orm.BoundInclusive, hi, orm.BoundExclusive)
```

Reading one back:

```go
lo, loKind := r.LowerBound()
hi, hiKind := r.UpperBound()
if r.IsEmpty() { /* ... */ }
```

## Declaring a column

```go
//orm:table public.bookings
type Booking struct {
    ID     int64          `orm:"pk,identity"`
    During orm.Range[time.Time] `orm:"pgtype:tstzrange"`
    Prices *orm.Range[int32]    `orm:"pgtype:int4range"`
}
```

Which range type a `Range[time.Time]` is — `daterange`, `tsrange` or `tstzrange`
— comes from the catalog rather than being guessed from Go, which is why the tag
names it.

## Querying

```go
db.Bookings.Query().Where(Bookings.During.Overlaps(r))         // &&
db.Bookings.Query().Where(Bookings.During.Contains(t))         // @> a value
db.Bookings.Query().Where(Bookings.During.ContainsRange(r))    // @> a range
db.Bookings.Query().Where(Bookings.During.ContainedBy(r))      // <@
db.Bookings.Query().Where(Bookings.During.Adjacent(r))         // -|-
db.Bookings.Query().Where(Bookings.During.StrictlyLeftOf(r))   // <<
db.Bookings.Query().Where(Bookings.During.StrictlyRightOf(r))  // >>
db.Bookings.Query().Where(Bookings.During.NotLeftOf(r))        // &>
db.Bookings.Query().Where(Bookings.During.NotRightOf(r))       // &<
```

`Contains` takes a value, `ContainsRange` takes a range. They are different
operators in PostgreSQL and different methods here, so the one you meant is the
one you get.

Comparing against another column of the same entity:

```go
Bookings.During.OverlapsCol(Bookings.Requested)
Bookings.During.ContainsCol(Bookings.Requested)
```

## Reading the bounds in SQL

```go
Bookings.During.Lower()      // lower(during)     -> *T
Bookings.During.Upper()      // upper(during)     -> *T
Bookings.During.LowerInc()   // lower_inc(during) -> bool
Bookings.During.LowerInf()   // lower_inf(during) -> bool
Bookings.During.IsEmpty()    // isempty(during)   -> bool
```

`Lower` and `Upper` are nullable, because an unbounded end has no value.

To filter on emptiness, use the predicate form rather than comparing the value:

```go
db.Bookings.Query().Where(Bookings.During.IsEmptyIs(true))
```

## Multiranges

A multirange is an ordered set of non-overlapping ranges — what you get when you
union two ranges that do not touch.

```go
//orm:table public.schedules
type Schedule struct {
    Free orm.Multirange[time.Time] `orm:"pgtype:tstzmultirange"`
}
```

```go
Schedules.Free.Contains(t)                 // a value
Schedules.Free.ContainsRange(r)            // one range
Schedules.Free.ContainsMultirange(m)       // a whole multirange
Schedules.Free.Overlaps(m)
Schedules.Free.OverlapsRange(r)
Schedules.Free.Merge()                     // range_merge -> Range[T]
Schedules.Free.IsEmpty()
```

`Merge` collapses a multirange to the single range spanning it — the smallest
range containing every member, gaps included.

## What PostgreSQL canonicalises

Discrete ranges — `int4range`, `int8range`, `daterange` — come back in canonical
form, so `[1,10]` arrives as `[1,11)`. Every multirange is canonicalised too.
That is the server's normalisation, not this package's, and the values you read
are the values it holds.

## Worked examples

### A meeting room

Double booking is one predicate, not a pair of comparisons you have to get right:

```go
wanted := orm.ClosedOpen(start, end)

clash, err := db.Bookings.Query().
    Where(Bookings.RoomID.Eq(roomID)).
    Where(Bookings.During.Overlaps(wanted)).
    Exists(ctx)
```

`ClosedOpen` is the right shape for time: a booking ending at 10:00 and one
starting at 10:00 do not overlap, and `[start, end)` is what says so.

### A price with a validity window

The price in force on a date, and the rows that have no end yet:

```go
current, err := db.Tariffs.Query().
    Where(Tariffs.ProductID.Eq(id)).
    Where(Tariffs.Valid.Contains(on)).
    One(ctx)

open, err := db.Tariffs.Query().
    Where(Tariffs.Valid.Overlaps(orm.RangeFrom(time.Now()))).
    All(ctx)
```

### A rota

Where cover ends and the next shift has not started — adjacency and gaps:

```go
// Shifts that touch without overlapping.
db.Shifts.Query().Where(Shifts.Hours.Adjacent(other))

// Everything entirely before a cutoff.
db.Shifts.Query().Where(Shifts.Hours.StrictlyLeftOf(orm.RangeFrom(cutoff)))

// The bounds, read in SQL.
var span = orm.Project2(
    Shifts.Hours.Lower(), Shifts.Hours.Upper(),
    func(from, to *time.Time) Span { return Span{from, to} },
)
```

Both bounds are nullable because an open-ended shift has no value there.

### Availability as a multirange

```go
// Any of the free windows covers the whole appointment.
db.Calendars.Query().Where(Calendars.Free.ContainsRange(appointment))

// The span from first free minute to last, gaps included.
var span = orm.Project1(
    Calendars.Free.Merge(),
    func(r orm.Range[time.Time]) orm.Range[time.Time] { return r },
)
```
