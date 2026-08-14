---
title: Queries
description: Reading entities: filters, ordering, paging and the terminal operations.
---

## The shape

```go
users, err := db.Users.Query().
    Where(Users.Active.Eq(true)).
    OrderBy(Users.CreatedAt.Desc()).
    Limit(50).
    All(ctx)
```

A `Query` is mutable and single-use. `Clone` branches one when you want a base:

```go
base := db.Users.Query().Where(Users.Active.Eq(true))
recent := base.Clone().Where(Users.CreatedAt.Gte(cutoff))
count, _ := base.Clone().Count(ctx)
```

## Terminals

| Method | Returns |
| --- | --- |
| `All(ctx)` | `[]E` |
| `One(ctx)` | `E`, or `ErrNotFound` |
| `Count(ctx)` | `int64` |
| `Exists(ctx)` | `bool` |
| `Rows(ctx)` | `iter.Seq2[E, error]` — streams |
| `SQL()` | the statement and its arguments, without running it |

Builder mistakes accumulate and surface together from the terminal, so a query that cannot be built never reaches PostgreSQL:

```go
_, err := db.Users.Query().Where(broken).OrderBy(alsoBroken).All(ctx)
// err reports both, not just the first
```

## Where

Multiple `Where` calls are ANDed. That is the common case and it keeps dynamic filtering readable:

```go
q := db.Users.Query()
if email != "" {
    q = q.Where(Users.Email.ILike("%" + email + "%"))
}
if onlyActive {
    q = q.Where(Users.Active.Eq(true))
}
users, err := q.All(ctx)
```

For OR, combine explicitly:

```go
db.Users.Query().Where(orm.Or(
    Users.Email.Eq("a@example.com"),
    Users.Email.Eq("b@example.com"),
))
```

`orm.And()` over an empty slice produces a query with no `WHERE` at all rather than `WHERE TRUE`.

## Ordering and paging

```go
db.Users.Query().
    OrderBy(Users.CreatedAt.Desc(), Users.ID.Asc()).
    Limit(20).
    Offset(40)
```

`Limit(0)` is a legal query that returns nothing. Only a negative value is a mistake.

Keyset paging beats `OFFSET` on large tables, and the typed API expresses it directly:

```go
db.Users.Query().
    Where(orm.Or(
        Users.CreatedAt.Lt(lastSeenAt),
        orm.And(Users.CreatedAt.Eq(lastSeenAt), Users.ID.Lt(lastSeenID)),
    )).
    OrderBy(Users.CreatedAt.Desc(), Users.ID.Desc()).
    Limit(20)
```

## Streaming

`Rows` yields as the server sends, so a large result never all exists at once:

```go
for user, err := range db.Users.Query().Rows(ctx) {
    if err != nil {
        return err
    }
    if err := handle(user); err != nil {
        return err
    }
}
```

A relation needing a statement of its own would have to see every row before it could run — which is the one thing streaming exists to avoid — so `Rows` refuses `With` rather than quietly buffering.

## Locking

```go
db.Users.Query().Where(Users.ID.Eq(id)).ForUpdate()
db.Users.Query().Lock(orm.ForUpdateStrong, orm.SkipLocked())
db.Users.Query().Lock(orm.ForShare, orm.NoWait())
```

Locking the nullable side of an outer join is something PostgreSQL refuses, so when the statement has joins the lock names the root table explicitly.

## Seeing the SQL

```go
sql, args, err := db.Users.Query().Where(Users.Active.Eq(true)).SQL()
// SELECT "users"."id", ... FROM "public"."users" WHERE "users"."active" = $1
// args: [true]
```

Values are never in the SQL. Every one is a bind parameter, including the ones inside `Expr` fragments.
