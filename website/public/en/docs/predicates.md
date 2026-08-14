# Predicates

> The comparisons each column type offers, and why the set differs.

Source: https://ormgo.vercel.app/en/docs/predicates/
Symbols: https://ormgo.vercel.app/api/orm.txt — the generated list of every exported name.

---
## The set depends on the type

A predicate exists on a descriptor when PostgreSQL defines the operation for that type. That is why the lists differ, and why the difference is a compile error rather than a runtime one.

### Every column

```go
Users.Email.Eq("a@example.com")
Users.Email.Ne("a@example.com")
Users.Email.In("a@example.com", "b@example.com")
Users.ID.In(ids...)          // for a slice you already have
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
orm.Not(Users.Email.Like("%@spam.test"))
Users.Email.Like("admin%")
Users.Email.Like("%.org")
Users.Email.Like("%example%")
```

### Nullable columns

Only nullable columns have these, because on a `NOT NULL` column they answer a question that cannot arise:

```go
Users.Bio.IsNull()
Users.Bio.IsNotNull()
Users.Bio.Eq("hello")   // still available: it means bio = 'hello'
```

### Arrays

Array containment is free functions rather than methods, and they produce a
`Predicate[Composed]`. A non-nullable column is lifted with `orm.Opt`:

```go
orm.ArrayContains(orm.Opt(Users.Tags), orm.Val([]string{"go"}))     // @>
orm.ArrayContainedBy(orm.Opt(Users.Tags), orm.Val(all))             // <@
orm.ArrayOverlaps(orm.Opt(Users.Tags), orm.Val([]string{"a", "b"})) // &&
```

### JSONB

Also free functions, for the same reason — either side can be a column or an
expression. See [JSON and JSONB](/en/docs/json/) for the whole set:

```go
orm.JSONHasKey(orm.Opt(Users.Meta), "plan")
orm.JSONPathExists(orm.Opt(Users.Meta), "$.billing.tier")
orm.JSONPathText(orm.Opt(Users.Meta), "billing", "tier")
```

### Ranges

```go
Bookings.During.Overlaps(r)
Bookings.During.Contains(t)
Bookings.During.StrictlyLeftOf(other)
Bookings.During.Adjacent(other)
```

### Full text

```go
orm.Matches(Docs.Search, orm.PlainToTSQuery(orm.English, "postgres mapper"))
orm.TSRank(Docs.Search, query).Desc()
```

## Combining

```go
orm.And(a, b, c)
orm.Or(a, b)
orm.Not(a)
```

They nest, and the compiler keeps them on one entity: an `orm.And` mixing a `Predicate[User]` and a `Predicate[Order]` does not compile. That is not pedantry — such a predicate would produce SQL naming a table the statement never introduced.

## Comparing two columns

The column-to-column comparisons are free functions rather than methods, and
they produce a `Predicate[Composed]` — so they belong in a composed query rather
than in an entity `Where`:

```go
orm.Compose(pool, shape).
    From(Orders.Source()).
    Where(orm.Gt(Orders.Total, Orders.Paid))
```

`Eq`, `Ne`, `Gt`, `Gte`, `Lt` and `Lte` all take two typed values, and both sides
must carry the same value type. The right-hand side can be an expression:

```go
orm.Eq(Orders.Total, Orders.Net.AddCol(Orders.Tax))
```

Arithmetic on a column is a method — `Add`, `Sub`, `Mul`, `Div` against a value,
and `AddCol` or `SubCol` against another column of the same entity.

## Raw fragments

When something has no typed form:

```go
db.Users.Query().Where(orm.Expr[User]("age(created_at) > interval ?", "1 year"))
```

`Expr` takes SQL text deliberately. It does not take values formatted into it — every `?` becomes a bind parameter, and the fragment's placeholders are validated against the arguments given.

## Worked examples

### A job board

Three filters that read as three sentences, and one that does not exist in SQL
until you write it.

```go
// Remote roles, posted this month, paying at least the floor.
db.Postings.Query().Where(
    Postings.Remote.Eq(true),
    Postings.PostedAt.Gte(monthStart),
    Postings.SalaryMin.Gte(60000),
)

// Anything but the agencies we have blocked.
db.Postings.Query().Where(orm.Not(Postings.CompanyID.In(blocked...)))

// Title or description — one OR, written once.
db.Postings.Query().Where(orm.Or(
    Postings.Title.ILike("%golang%"),
    Postings.Description.ILike("%golang%"),
))
```

### A moderation queue

The NULL cases, which are where most filter bugs live:

```go
// Never reviewed: reviewed_at was never set.
db.Comments.Query().Where(Comments.ReviewedAt.IsNull())

// Reviewed and cleared: set, and no reason recorded.
db.Comments.Query().Where(
    Comments.ReviewedAt.IsNotNull(),
    Comments.RejectReason.IsNull(),
)

// Reviewed and rejected with a reason that is not the empty string.
db.Comments.Query().Where(
    Comments.RejectReason.IsNotNull(),
    orm.Not(Comments.RejectReason.Eq("")),
)
```

A `NOT NULL` column has no `IsNull`, so the first two cannot be written against
one by mistake.

### A price book

Comparing two columns, which is a composed query rather than an entity one:

```go
// Anything currently sold below cost.
orm.Compose(pool, shape).
    From(Prices.Source()).
    Where(orm.Lt(Prices.Retail, Prices.Cost)).
    All(ctx)

// Margin below a threshold, computed rather than stored.
orm.Compose(pool, shape).
    From(Prices.Source()).
    Where(orm.Lt(Prices.Retail.SubCol(Prices.Cost), orm.Val(int32(500)))).
    All(ctx)
```
