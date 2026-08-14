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

## RETURNING

PostgreSQL can hand back the rows a write touched. This library uses it in three
different ways, and the difference is worth knowing because two of them are
automatic and one is not.

### Insert always returns

```go
user, err := db.Users.Insert(ctx, User{Email: "a@example.com"})
// user.ID is set; so is CreatedAt, and anything else the database filled in
```

That is why `Insert` returns `(E, error)` rather than an error alone. An
identity key, a `DEFAULT now()`, a generated column and a trigger's edits all
arrive in the returned value, so the struct you get back is the row as it exists
— not the struct you sent.

`InsertMany` does the same for a slice, in order:

```go
users, err := db.Users.InsertMany(ctx, []User{a, b, c})
// users[1].ID is b's key
```

The column list is always explicit. A `RETURNING *` would decide the scan order
at the server, where the generated scanner cannot see it.

### Upsert returns the surviving row

```go
user, err := db.Users.Insert(ctx, incoming,
    orm.OnConflict(Users.Email).DoUpdate(Users.Name, Users.Seen))
```

Whether it inserted or updated, what comes back is the row that is now in the
table. That is the usual reason to reach for `DoUpdate` over `DoNothing`:
`DoNothing` on a conflict returns **no row**, so the value you get is the zero
entity and you cannot tell "already there" from "just written" by looking at it.

### Update and delete do not, unless you ask

`Exec` returns a count:

```go
n, err := db.Users.Update(ctx).
    Set(Users.Active, false).
    Where(Users.CreatedAt.Lt(cutoff)).
    Exec(ctx)
// n is how many rows changed
```

A count answers "how many". When you need "which", wrap the builder:

```go
updated, err := orm.UpdateReturningEntity(
    db.Users.Update(ctx).Set(Users.Active, false).Where(Users.CreatedAt.Lt(cutoff)),
).All(ctx)
// []User — every row that matched, as it is after the update
```

```go
deleted, err := orm.DeleteReturningEntity(
    db.Users.Delete(ctx).Where(Users.ID.Eq(id)),
).One(ctx)
// the row as it was, immediately before it stopped existing
```

Note the difference in tense. An update returns the **new** values; a delete
returns the row that is now gone. Both are the only chance you get: after the
statement, one of them cannot be queried and the other no longer holds the old
values.

### Returning a shape rather than the entity

When you only need two columns of what changed:

```go
type Changed struct {
    ID    int64
    Email string
}

var changed = orm.Project2(
    Users.ID, Users.Email,
    func(id int64, email string) Changed { return Changed{id, email} },
)

rows, err := orm.UpdateReturning(
    db.Users.Update(ctx).Set(Users.Active, false).Where(cond),
    changed,
).All(ctx)
// []Changed
```

`DeleteReturning` takes a projection the same way.

### The terminals

A `Returning` offers three, and no others:

| Method | For |
| --- | --- |
| `All(ctx)` | every row the write touched |
| `One(ctx)` | exactly one, or `ErrNotFound`; more than one is an error |
| `SQL()` | the statement and its arguments, without running it |

There is no `Exec` on a `Returning`, because a statement whose rows you asked
for and then discarded is a statement that wanted `Exec` in the first place.

### It is still a write

`ErrMissingWhere` applies exactly as it does without `RETURNING` — wrapping an
update does not make an unconditional one safe:

```go
_, err := orm.UpdateReturningEntity(
    db.Users.Update(ctx).Set(Users.Active, false),
).All(ctx)
// errors.Is(err, orm.ErrMissingWhere)
```

And it is one statement. The rows come back from the write itself, not from a
`SELECT` afterwards — which is what makes them the rows that write touched, even
under concurrency, rather than the rows that match now.

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

