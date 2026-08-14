---
title: Writing data
description: Insert, update, delete and COPY — all of it explicit.
---

## Insert

```go
user, err := db.Users.Insert(ctx, User{
    Email: "a@example.com",
    Active: true,
})
// user.ID is populated from RETURNING
```

Many at once:

```go
users, err := db.Users.InsertMany(ctx, []User{u1, u2, u3})
```

## Defaults

A Go zero value is a value. Asking for the column's default is separate and explicit:

```go
db.Users.Insert(ctx, User{}, orm.Default(Users.Active, Users.CreatedAt))
```

The named columns are left out of the `INSERT` entirely, so the column's `DEFAULT` applies — or its sequence, or NULL for a nullable column with no default.

## Update

```go
n, err := db.Users.Update(ctx).
    Set(Users.Active, false).
    Where(Users.CreatedAt.Lt(cutoff)).
    Exec(ctx)
```

An update with no `WHERE` is refused:

```go
_, err := db.Users.Update(ctx).Set(Users.Active, false).Exec(ctx)
// errors.Is(err, orm.ErrMissingWhere)
```

Unless you say every row was meant:

```go
db.Users.Update(ctx).Set(Users.Active, false).All().Exec(ctx)
```

Set from an expression rather than a value:

```go
db.Orders.Update(ctx).
    SetExpr(Orders.Total, Orders.Net.AddCol(Orders.Tax)).
    Where(Orders.ID.Eq(id))
```

## Delete

```go
n, err := db.Users.Delete(ctx).Where(Users.ID.Eq(id)).Exec(ctx)
```

Same `ErrMissingWhere` rule, for the same reason.

## Upsert

```go
db.Users.Insert(ctx, user,
    orm.OnConflict(Users.Email).DoUpdate(
        orm.Assign(Users.Active, true),
    ),
)

db.Users.Insert(ctx, user, orm.OnConflict(Users.Email).DoNothing())
```

The conflict target is a column list PostgreSQL matches against; `DO UPDATE` sees the row that conflicted and `EXCLUDED`.

## COPY

For bulk loading, `COPY` is an order of magnitude faster than `INSERT`:

```go
n, err := db.Events.CopyFrom(ctx, events)
```

Streaming, so the rows never all exist at once:

```go
n, err := db.Events.CopyFromSeq(ctx, func(yield func(Event, error) bool) {
    for scanner.Scan() {
        ev, err := parse(scanner.Text())
        if !yield(ev, err) {
            return
        }
    }
})
```

A subset of columns:

```go
n, err := orm.CopyColumns(ctx, db.Events, events, Events.ID, Events.Kind)
```

A failing `COPY` fails as one statement — no part of it is applied. If it has to succeed together with other work, run it in a transaction.

## Returning a projection

```go
rows, err := db.Users.Insert(ctx, user, orm.Returning(Summaries))
```
