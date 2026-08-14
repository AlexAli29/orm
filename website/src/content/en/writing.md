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
    orm.OnConflict(Users.Email).DoUpdateSet(
        Users.Active.Set(true),
    ),
)

db.Users.Insert(ctx, user, orm.OnConflict(Users.Email).DoNothing())
```

The conflict target is a column list PostgreSQL matches against; `DO UPDATE` sees the row that conflicted and `EXCLUDED`.

## Upsert, in detail

`OnConflict` names the columns PostgreSQL matches a conflict against — usually a
unique constraint's columns.

```go
// Do nothing when it is already there.
db.Users.Insert(ctx, user, orm.OnConflict(Users.Email).DoNothing())

// Take the incoming row's values for the named columns.
db.Users.Insert(ctx, user, orm.OnConflict(Users.Email).DoUpdate(Users.Name, Users.Seen))

// Or set them yourself.
db.Users.Insert(ctx, user, orm.OnConflict(Users.Email).DoUpdateSet(
    Users.Seen.Set(time.Now()),
    Users.Hits.SetExpr(Users.Hits.Add(1)),
))
```

`DoUpdate` is the common case and reads as "these columns take the new values".
`DoUpdateSet` is for when the new value is computed — incrementing a counter,
keeping the larger of two numbers, appending to an array.

A partial index needs the same predicate on the conflict clause:

```go
orm.OnConflict(Users.Email).Where(Users.Active.Eq(true)).DoNothing()
```

## RETURNING on update and delete

An insert returns its rows already. Update and delete do not, so ask:

```go
updated, err := orm.UpdateReturningEntity(
    db.Users.Update(ctx).Set(Users.Active, false).Where(Users.ID.Eq(id)),
).All(ctx)
// []User — the rows as they are after the update

deleted, err := orm.DeleteReturningEntity(
    db.Users.Delete(ctx).Where(Users.ID.Eq(id)),
).One(ctx)
```

Or return a projection rather than the whole entity:

```go
rows, err := orm.UpdateReturning(
    db.Users.Update(ctx).Set(Users.Active, false).Where(cond),
    Summaries,
).All(ctx)
```

This is how you learn what a conditional write actually did — the row count tells
you how many, and `RETURNING` tells you which.

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
