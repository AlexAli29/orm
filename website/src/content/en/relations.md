---
title: Relations
description: Loading what you asked for, in a statement count you can predict.
---

## Declaring

```go
//orm:table public.users
type User struct {
    ID     int64 `orm:"pk,identity"`
    Orders orm.Many[Order]
}

//orm:table public.orders
type Order struct {
    ID     int64 `orm:"pk,identity"`
    UserID int64
    User   orm.One[User] `orm:"fk:user_id"`
}
```

The foreign key is named on the side that holds it. The generator checks it exists and points where you said.

## Loading

```go
users, err := db.Users.Query().With(Users.Orders).All(ctx)
```

`With` loads what it is given and nothing else. There is no lazy loading, so a loop over the result cannot become a query per row.

## Predictable statement counts

Loading is breadth-first and batched. The number of statements follows the shape of the tree you asked for, never the number of rows in it:

```go
db.Users.Query().
    With(Users.Orders.With(Orders.Items)).
    All(ctx)
// three statements: users, then all their orders, then all those items
```

Ten users or ten thousand, it is three.

## Configuring a relation

`Rel` carries options — and they apply per parent, which is what makes "the five most recent orders for each user" one statement rather than N:

```go
db.Users.Query().
    With(Users.Orders.
        Where(Orders.Status.Eq("paid")).
        OrderBy(Orders.Placed.Desc()).
        Limit(5)).
    All(ctx)
```

## Filtering by a relation without loading it

```go
// users who have at least one paid order
db.Users.Query().Where(Users.Orders.Any(Orders.Status.Eq("paid")))

// users with none
db.Users.Query().Where(Users.Orders.None(Orders.Status.Eq("refunded")))
```

These compile to semi-joins. They load nothing, so they cost nothing to read.

## Reading the result

```go
for _, u := range users {
    orders, ok := u.Orders.Get()
    if !ok {
        // not loaded — this is different from "loaded and empty"
        continue
    }
    fmt.Println(len(orders))
}
```

Three states, distinguishable: unloaded, loaded and empty, loaded and present. The zero value is unloaded, so a struct literal that omits a relation says "I did not ask" rather than "there is nothing".

## What decides relatedness

PostgreSQL does. Rows relate by what the database says equal keys are, so `citext`, `numeric`, domains and composite keys behave as they do in the database rather than as Go equality would.
