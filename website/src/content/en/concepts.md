---
title: Core concepts
description: The vocabulary the rest of the documentation uses.
---

## Entity

A Go struct marked with `//orm:table`. You write it; nothing generates it.

## Descriptor

The generated, typed handle for a column: `Users.Email`, `Orders.Placed`. Its Go type encodes what the catalog said — the value type, whether it is nullable, and which comparisons PostgreSQL defines for it.

```go
Users.Email   // orm.TextCol[User]              — has Like, ILike
Users.ID      // orm.OrdCol[User, int64]        — has Gt, Between, Asc
Users.Bio     // orm.NullTextCol[User]          — has IsNull
Users.Tags    // orm.Col[User, []string]        — equality only
```

Descriptors are read-only and safe to share. Anything that looks like mutation — aliasing a table, configuring a relation — returns a copy.

## Capability

What a column type can do in SQL, not what Go can do with it. `uuid` is ordered because PostgreSQL orders it; `jsonb` is not, because comparing two jsonb documents answers no question anybody asks. This is why the ORM does not key ordering off Go's `cmp.Ordered`.

## Source

One occurrence of a relation in a statement. Aliasing a table produces a second source, and a descriptor built from one source cannot be used against another — which is what makes a self-join safe.

## Repo

`Repo[E]` binds generated metadata to an executor: a `*pgxpool.Pool`, a `*pgx.Conn` or a `pgx.Tx`. Generated code gives you a `DB` struct holding one per entity.

## Query, SelectQuery, ComposedQuery

Three builders, three jobs:

- `Query[E]` reads whole entities from one table.
- `SelectQuery[E, R]` reads a projection — a chosen result shape — from one table.
- `ComposedQuery[R]` reads a projection from sources you composed yourself: joins, CTEs, derived tables.

All three are mutable, single-use and not safe for concurrent use. `Clone` branches one.

## Projection

A typed result shape: what to select, and how to read it back.

```go
type Summary struct {
    ID    int64
    Email string
}

var Summaries = orm.Project2(
    Users.ID, Users.Email,
    func(id int64, email string) Summary { return Summary{ID: id, Email: email} },
)
```

The arity is written out — `Project1` through `Project8` — because Go cannot express "a list of expressions whose types are all different and all remembered". What it buys is a row hot path with no reflection and no `[]any`.

## Nullability, and where it comes from

Two things make a value nullable, and they are different:

1. **The column** is nullable. `Users.Bio` is `NullTextCol`.
2. **The query** makes it nullable. A `NOT NULL` column read through a `LEFT JOIN` can be NULL for a row that matched nothing.

The second is source-induced nullability. It is why `orm.Opt` exists, and why a select list that reads an outer-joined source with `orm.Of` is refused.

## Error handling

Sentinels are wrapped rather than replaced, so `errors.Is` works through the context each layer adds, and PostgreSQL's own `*pgconn.PgError` stays reachable with `errors.As`. No exported API panics for a bad query, a database error or a failed scan.
