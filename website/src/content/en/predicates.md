---
title: Predicates
description: The comparisons each column type offers, and why the set differs.
---

## The set depends on the type

A predicate exists on a descriptor when PostgreSQL defines the operation for that type. That is why the lists differ, and why the difference is a compile error rather than a runtime one.

### Every column

```go
Users.Email.Eq("a@example.com")
Users.Email.Ne("a@example.com")
Users.Email.In("a@example.com", "b@example.com")
Users.ID.InSlice(ids)          // for a slice you already have
```

There is deliberately no `NotIn`. `orm.Not(Users.ID.In(...))` says the same thing and says it once.

### Ordered columns

Any type PostgreSQL orders — integers, floats, text, dates, `uuid`, `inet`, `interval`:

```go
Users.CreatedAt.Gt(t)
Users.CreatedAt.Gte(t)
Users.CreatedAt.Lt(t)
Users.CreatedAt.Lte(t)
Users.CreatedAt.Between(from, to)
Users.CreatedAt.Asc()
Users.CreatedAt.Desc()
```

`jsonb` and `bytea` have a total order for indexing, but comparing two of them answers no question anybody asks, so they stay at equality.

### Text columns

```go
Users.Email.Like("%@example.com")
Users.Email.ILike("%@EXAMPLE.com")
Users.Email.NotLike("%@spam.test")
Users.Email.HasPrefix("admin")
Users.Email.HasSuffix(".org")
Users.Email.Contains("example")
```

### Nullable columns

Only nullable columns have these, because on a `NOT NULL` column they answer a question that cannot arise:

```go
Users.Bio.IsNull()
Users.Bio.IsNotNull()
Users.Bio.Eq("hello")   // still available: it means bio = 'hello'
```

### Arrays

```go
Users.Tags.Contains([]string{"go"})     // @>
Users.Tags.ContainedBy(all)             // <@
Users.Tags.Overlaps([]string{"a", "b"}) // &&
Users.Tags.HasElement("go")             // = ANY
Users.Tags.Len().Eq(3)
```

### JSONB

```go
Users.Meta.HasKey("plan")
Users.Meta.Contains(orm.JSONB(`{"plan":"pro"}`))
Users.Meta.Path("billing", "tier").AsText().Eq("gold")
```

### Ranges

```go
Bookings.During.Overlaps(r)
Bookings.During.ContainsElement(t)
Bookings.During.StrictlyLeftOf(other)
Bookings.During.Adjacent(other)
```

### Full text

```go
Docs.Search.Matches(orm.PlainToTSQuery("english", "postgres mapper"))
Docs.Search.Rank(query).Desc()
```

## Combining

```go
orm.And(a, b, c)
orm.Or(a, b)
orm.Not(a)
```

They nest, and the compiler keeps them on one entity: an `orm.And` mixing a `Predicate[User]` and a `Predicate[Order]` does not compile. That is not pedantry — such a predicate would produce SQL naming a table the statement never introduced.

## Expressions as values

Comparing two columns, or a column to an expression:

```go
Orders.Total.GtCol(Orders.Paid)
Orders.Total.EqOf(orm.Add(Orders.Net, Orders.Tax))
```

## Raw fragments

When something has no typed form:

```go
db.Users.Query().Where(orm.Expr[User]("age(created_at) > interval ?", "1 year"))
```

`Expr` takes SQL text deliberately. It does not take values formatted into it — every `?` becomes a bind parameter, and the fragment's placeholders are validated against the arguments given.
