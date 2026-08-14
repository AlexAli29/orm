---
title: Projections
description: Selecting the columns you want, into a type you chose.
---

## What a projection is

A projection is two things bundled together:

1. **which expressions to select**, and
2. **a function that turns those values into your result type**.

That is the whole idea. Everything below is that one idea with more columns.

## Start with one column

An entity query gives you whole entities:

```go
users, err := db.Users.Query().All(ctx)
// []User — SELECT id, email, bio, active, created_at FROM users
```

Suppose you only want the email addresses. Say so:

```go
var Emails = orm.Project1(
    Users.Email,                        // select this
    func(email string) string {         // and hand it back like this
        return email
    },
)

emails, err := orm.Select(db.Users, Emails).All(ctx)
// []string — SELECT email FROM users
```

`[]string`, not `[]User`. The database sent one column instead of five, and the result type is whatever your function returned.

## Two columns, into a struct

The result type is yours. Usually it is a struct you declared:

```go
type Summary struct {
    ID    int64
    Email string
}

var Summaries = orm.Project2(
    Users.ID,       // 1st expression
    Users.Email,    // 2nd expression
    func(id int64, email string) Summary {
        //   ↑ 1st parameter   ↑ 2nd parameter
        return Summary{ID: id, Email: email}
    },
)

rows, err := orm.Select(db.Users, Summaries).All(ctx)
// []Summary — SELECT id, email FROM users
```

**Read the call top to bottom.** `Project2` takes two expressions, so the function takes two parameters, in the same order. The first parameter is the first column, the second is the second.

**The parameter types are not your choice.** `Users.ID` is a `bigint`, so the first parameter must be `int64`. `Users.Email` is `text`, so the second must be `string`. Write `func(id string, ...)` and it does not compile — the mismatch is caught where you wrote it, not when a row arrives.

That is why the number is in the name: `Project1` for one expression, `Project2` for two, up to `Project8`.

## Anything that produces a value

An expression does not have to be a plain column. An aggregate is an expression:

```go
type ByStatus struct {
    Status string
    Count  int64
}

var byStatus = orm.Project2(
    Orders.Status,        // a column
    orm.Count[Order](),   // count(*)
    func(status string, n int64) ByStatus {
        return ByStatus{Status: status, Count: n}
    },
)

rows, err := orm.Select(db.Orders, byStatus).
    GroupBy(Orders.Status).
    All(ctx)
// []ByStatus — SELECT status, count(*) FROM orders GROUP BY status
```

`orm.Count[Order]()` returns `int64`, so the second parameter is `int64`. Same rule as before.

## Where a projection can be used

The projection says *what to select*. Something else says *where from*:

```go
orm.Select(db.Users, Summaries)                     // from the users table
orm.SelectFrom(db.Users, Users.As("u"), Summaries)  // from an alias of it
orm.Compose(pool, shape)                            // from sources you joined
```

A `Projection` is a value, not a query. Build it once at package level and use it from as many queries as you like — it is immutable and safe to share.

```go
// one shape, three queries
active, _ := orm.Select(db.Users, Summaries).Where(Users.Active.Eq(true)).All(ctx)
recent, _ := orm.Select(db.Users, Summaries).OrderBy(Users.ID.Desc()).Limit(10).All(ctx)
count,  _ := orm.Select(db.Users, Summaries).Count(ctx)
```

## When to reach for one

| Use | When |
| --- | --- |
| An entity query | You want the row and most of its columns |
| A projection | You want a few columns, an aggregate, or a shape that is not a table row |

A projection is also the only way to select something that is not a column at all — a count, a sum, an expression, a value from a CTE.

## Aggregates

```go
orm.Count[User]()                 // count(*)      -> int64
orm.CountOf(Users.Bio)            // count(bio)    -> int64
orm.Max(Orders.Total)             // max(total)    -> *T
orm.Min(Orders.Placed)            // min(placed)   -> *T
orm.SumInt32(Orders.Qty)          // sum(qty)      -> *int64
orm.AvgInt64(Orders.Qty)          // avg(qty)      -> *N
orm.SumNumeric[Order, Decimal](Orders.Total)
```

Most of them return a **pointer**, and that is not caution. `max` over no rows is NULL, and a non-pointer result would have nowhere to put that:

```go
var maxTotal = orm.Project1(
    orm.Max(Orders.Total),
    func(v *int64) *int64 { return v },  // nil when the table is empty
)
```

`count` is the exception — over no rows it is zero, so it is a plain `int64`.

## Grouping and having

```go
orm.Select(db.Orders, byStatus).
    Where(Orders.Placed.Gte(cutoff)).
    GroupBy(Orders.Status).
    Having(orm.Count[Order]().Gt(100)).
    OrderBy(Orders.Status.Asc()).
    All(ctx)
```

Group order is yours and is never sorted — it decides the grouping PostgreSQL performs.

## Distinct

```go
orm.Select(db.Orders, shape).Distinct()
orm.Select(db.Orders, shape).DistinctOn(Orders.UserID)
```

`DISTINCT ON` keeps the first row of each group of equal values, which is a different clause from `DISTINCT`. The two cannot both be set, and the builder says so rather than emitting something PostgreSQL rejects.

## Naming the outputs

When a projection becomes a derived table or a CTE, its columns need names, because something outside will refer to them:

```go
userID := orm.Named("user_id", orm.Of(Orders.UserID))
total  := orm.Named("total", orm.Count[orm.Composed]())
```

The name is required rather than derived. `count(*)` has none, and inventing one from the rendered expression would make a derived table's column depend on how the compiler happened to spell it. See [Composition](/en/docs/composition/).

## Why there are eight constructors

This is the one part that is about Go rather than about SQL, and it is here for the curious rather than because you need it.

Go cannot express "a list of expressions whose result types are all different and all remembered". A variadic parameter has one type, and a type-parameter pack does not exist. Libraries that pretend otherwise do it with `[]any` and runtime assertions, which moves the mistake from the compiler to the customer.

Writing the arity out — `Project1` through `Project8` — is what buys the checking above, and it is also what makes the row hot path do no reflection, hold no map and assert nothing. Scanning is N typed locals, one `Scan`, and one call.

## Worked examples

### A billing report

Revenue per plan, for one month, with the count beside it — the shape a finance
page actually wants:

```go
type PlanRevenue struct {
    Plan    string
    Charges int64
    Total   *int64
}

var planRevenue = orm.Project3(
    Invoices.Plan,
    orm.Count[Invoice](),
    orm.SumInt32(Invoices.AmountCents),
    func(plan string, n int64, total *int64) PlanRevenue {
        return PlanRevenue{Plan: plan, Charges: n, Total: total}
    },
)

rows, err := orm.Select(db.Invoices, planRevenue).
    Where(Invoices.IssuedAt.Between(monthStart, monthEnd)).
    GroupBy(Invoices.Plan).
    OrderBy(Invoices.Plan.Asc()).
    All(ctx)
```

`Total` is `*int64` because `sum` over no rows is NULL, and a plan with no
invoices in the window is exactly that. The count beside it is not a pointer,
because `count` over no rows is zero.

### A device roster

One column, into a slice — no struct, because there is nothing to hold:

```go
var serials = orm.Project1(
    Devices.Serial,
    func(s string) string { return s },
)

offline, err := orm.Select(db.Devices, serials).
    Where(Devices.LastSeenAt.Lt(cutoff)).
    OrderBy(Devices.Serial.Asc()).
    All(ctx)
// []string
```

### A seating chart

Two columns into a map key, because the result type is whatever the function
returns — it does not have to be a struct:

```go
type Seat struct{ Row, Number int32 }

var seats = orm.Project2(
    Tickets.SeatRow, Tickets.SeatNumber,
    func(r, n int32) Seat { return Seat{Row: r, Number: n} },
)

taken, err := orm.Select(db.Tickets, seats).
    Where(Tickets.EventID.Eq(eventID)).
    Where(Tickets.CancelledAt.IsNull()).
    All(ctx)

occupied := make(map[Seat]bool, len(taken))
for _, s := range taken {
    occupied[s] = true
}
```
