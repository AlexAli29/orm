---
title: Projections
description: Reading a chosen result shape rather than a whole entity.
---

## Why they exist

Selecting an entity reads every mapped column. When you want two columns and a count, a projection says so — and reads it back into a type you named, with no reflection on the row path.

```go
type Summary struct {
    Email  string
    Orders int64
}

var Summaries = orm.Project2(
    Users.Email, orm.Count[User](),
    func(email string, n int64) Summary { return Summary{Email: email, Orders: n} },
)

rows, err := orm.Select(db.Users, Summaries).
    GroupBy(Users.Email).
    OrderBy(Users.Email.Asc()).
    All(ctx)
```

## The arity is written out

`Project1` through `Project8`. Go cannot express "a list of expressions whose result types are all different and all remembered" — a variadic has one type, and a type-parameter pack does not exist. Every library that pretends otherwise does it with `[]any` and runtime assertions, which moves the mistake from the compiler to the customer.

What the written-out arity buys: the row hot path does no reflection, holds no map and asserts nothing. Scanning is N typed locals, one `Scan`, and one call.

A `Projection` is a value. Build it once and use it from many queries; it is immutable and safe to share.

## Aggregates

```go
orm.Count[User]()                 // count(*)          -> int64
orm.CountOf(Users.Bio)            // count(bio)        -> int64
orm.Max(Orders.Total)             // max(total)        -> *T
orm.Min(Orders.Placed)            // min(placed)       -> *T
orm.SumInt32(Orders.Qty)          // sum(qty)          -> *int64
orm.AvgInt64(Orders.Qty)          // avg(qty)          -> *N
orm.SumNumeric[Order, Decimal](Orders.Total)
```

Most aggregates return a pointer, and that is not caution: `max` over no rows is NULL, and a non-pointer result would have nowhere to put that. `count` is the exception — it is zero.

## Grouping and having

```go
orm.Select(db.Orders, byUser).
    Where(Orders.Placed.Gte(cutoff)).
    GroupBy(Orders.UserID).
    Having(orm.Count[Order]().Gt(3)).
    OrderBy(Orders.UserID.Asc()).
    All(ctx)
```

Group order is yours and is never sorted — it decides the grouping PostgreSQL performs.

## Distinct

```go
orm.Select(db.Orders, shape).Distinct()
orm.Select(db.Orders, shape).DistinctOn(Orders.UserID)
```

`DISTINCT ON` keeps the first row of each group of equal values, which is a different clause from `DISTINCT` — the two cannot both be set, and the builder says so rather than emitting something PostgreSQL rejects.

## Naming the outputs

When a projection becomes a derived table or a CTE, its columns need names. That is `Named`, and the name is required rather than derived — `count(*)` has none, and inventing one from the rendered expression would make a derived table's column depend on how the compiler happened to spell it.

```go
userID := orm.Named("user_id", orm.Of(Orders.UserID))
total  := orm.Named("total", orm.Count[orm.Composed]())
```
