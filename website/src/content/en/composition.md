---
title: Composition
description: Joins, CTEs, derived tables and subqueries — one compiler, one statement.
---

## A typed query is a typed source

That is the whole idea. `Sub` makes a query a derived table, `CTE` makes it a `WITH` item, and `Compose` builds a statement over several of them. All of it nests through one compiler, so a statement with a CTE, a derived table, a correlated subquery and a window function has **one parameter list**, numbered in the order the SQL is written.

## Compose and join

```go
type Row struct {
    Email string
    Total *int64
}

shape := orm.Project2(
    orm.Of(Users.Email),
    orm.Opt(Orders.Total),
    func(email string, total *int64) Row { return Row{email, total} },
)

rows, err := orm.Compose(pool, shape).
    From(Users.Source()).
    LeftJoin(Orders.Source(), orm.Eq(Orders.UserID, Users.ID)).
    Where(orm.Cond(Users.Active.Eq(true))).
    OrderBy(orm.Of(Users.Email).Asc()).
    All(ctx)
```

Three lifting functions carry typed things into a composed query:

- `orm.Of(col)` — keeps the column's own type.
- `orm.Opt(col)` — the nullable form, for an outer-joined source.
- `orm.Cond(pred)` — an entity predicate as a composed one.

## Source-induced nullability

`orders.total` may be `NOT NULL` and still be NULL here, because the join can produce a row where the whole right-hand source is absent. So it widens, and reading it with `Of` is refused:

```text
select-list expression 2 reads public.orders, which an outer join can leave with
no row, into a result that cannot hold NULL

an outer join makes every value of that source nullable, whatever the column's
own constraint says; read it with Opt or OptRef, which widen the result type
```

That is a build-time refusal, not a scan error discovered on whichever row happened not to match.

## Derived tables

```go
userID := orm.Named("user_id", orm.Of(Orders.UserID))
count  := orm.Named("order_count", orm.Count[orm.Composed]())

stats := orm.Sub("post_stats", orm.Rows(userID, count).
    From(Orders.Source()).
    GroupBy(orm.Of(Orders.UserID)))

rows, err := orm.Compose(pool, shape).
    From(Users.Source()).
    LeftJoin(stats, orm.Eq(orm.Ref(stats, userID), orm.Of(Users.ID))).
    All(ctx)
```

`Ref(src, out)` reads a column of a row source, typed by the declaration rather than by a string. `OptRef` is its outer-join form.

## CTEs

```go
active := orm.CTE("active_users", orm.Rows(
    orm.Named("id", orm.Of(Users.ID)),
).From(Users.Source()).Where(orm.Cond(Users.Active.Eq(true))))

rows, err := orm.Compose(pool, shape).
    With(active).
    From(active).
    Join(Orders.Source(), orm.Eq(Orders.UserID, orm.Ref(active, id))).
    All(ctx)
```

The returned value is both the declaration, which `With` renders, and the reference, which `From` and the joins take. Aliasing it with `As` gives a second reference to the same item — which is how one CTE is joined to itself.

`Materialized()` and `NotMaterialized()` are available where the planner's estimate is wrong. Leaving it unset leaves the choice to the planner, which is the right default.

## Recursive CTEs

```go
tree := orm.RecursiveCTE("tree",
    anchor,     // the non-recursive term
    recursive,  // the term that refers to "tree"
)
```

This is the one place a `UNION` appears inside the ORM, because PostgreSQL's grammar requires it between a recursive CTE's anchor and its recursive term.

## Subqueries

```go
orm.Exists[User](sub)        // EXISTS (...)
orm.NotExists[User](sub)
orm.InSub(Users.ID, sub)          // id IN (SELECT ...)
orm.Scalar[User, int64](sub) // a scalar subquery — always nullable
```

A scalar subquery is always nullable, and that is PostgreSQL's semantics rather than caution: no row yields NULL, one row yields the value, and two rows are a run-time error the server raises.

## Scope is checked, sequentially

A reference to an occurrence the statement does not introduce is refused. The rules are SQL's own: a join condition sees the sources written before it and the one it attaches, and nothing to its right.

```go
// refused: c is joined after the condition that names it
q.From(a).Join(b, orm.Eq(b.X, c.X)).Join(c, ...)
```

Validating against the finished set of sources would accept that and let PostgreSQL complain later, in its vocabulary, about a column rather than about the order things were written in.

## One parameter list

```go
sql, args, _ := q.SQL()
// ... WHERE a = $1 AND b = $2 ... (SELECT ... WHERE c = $3) ... LIMIT ...
```

Nested statements share the writer, so numbering continues across every level. Nothing is rendered separately and concatenated.
