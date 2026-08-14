---
title: Expressions
description: Conditionals, coalescing, casts, string functions, and the escape hatch for the rest.
---

Everything here produces an `Expression` or a `Value`, which means it can go
anywhere one of those goes: a select list, a `WHERE`, an `ORDER BY`, a `GROUP BY`
or another expression.

## Literals

```go
orm.Val("pending")   // Expression[string, *string]
orm.Val(int64(0))
orm.Val(true)
```

A literal becomes a bind parameter, not text in the SQL. That is true of every
value in this package, and it is why none of these take a format string.

## CASE

```go
tier := orm.Case(orm.Cond(Orders.Total.Gte(1000)), orm.Val("gold")).
    When(orm.Cond(Orders.Total.Gte(100)), orm.Val("silver")).
    Else(orm.Val("bronze"))
```

```sql
CASE WHEN total >= $1 THEN $2 WHEN total >= $3 THEN $4 ELSE $5 END
```

`Case` takes the first condition and its result; `When` adds more; `Else` closes
it. The branches all carry one type, so a `CASE` mixing a string and an integer
does not compile.

**`Else` and `End` are different endings.** `Else` supplies a fallback, so the
result cannot be NULL and its type is `T`. `End` closes without one, so the
result is NULL when nothing matched and its type widens to `N`:

```go
grade := orm.Case(orm.Cond(Users.Score.Gte(90)), orm.Val("A")).End()
// Expression[*string, *string] — NULL for a score under 90
```

## COALESCE and NULLIF

```go
orm.Coalesce(Users.Nickname, orm.Of(Users.Email))
// the nickname, or the email when it is NULL -> string, never NULL
```

`Coalesce` takes a nullable value first and then the fallbacks. The result is
non-nullable, because the last fallback is not — that is the point of it.

`CoalesceNull` is the form where every input is nullable and the result may be
too.

```go
orm.NullIf(orm.Of(Users.Bio), orm.Val(""))
// NULL when bio is the empty string, otherwise bio
```

## Casts

```go
orm.Cast(Users.ID, orm.Text)          // id::text     -> string
orm.Cast(Users.Score, orm.BigInt)     // score::bigint
orm.CastNull(Users.Bio, orm.Text)     // the nullable form
```

The target is a `PGType` value rather than a string, so the Go result type is
decided by the cast rather than asserted afterwards. The built-in ones:

```go
orm.Text  orm.SmallInt  orm.Integer  orm.BigInt
orm.Boolean  orm.ByteA  orm.DoublePrecision
orm.Date  orm.Timestamptz
```

## String functions

```go
orm.Upper(Users.Email)
orm.Lower(Users.Email)
orm.Trim(Users.Name)
orm.Concat(orm.Of(Users.First), orm.Val(" "), orm.Of(Users.Last))
```

Each has a `…Null` form taking a nullable column and returning a nullable
result, because `upper(NULL)` is NULL.

## Arithmetic

Ordered columns carry the operators directly:

```go
Orders.Total.Add(10)
Orders.Total.Sub(10)
Orders.Total.Mul(2)
Orders.Total.Div(2)
Orders.Total.AddCol(Orders.Tax)   // column + column
```

## Anything else PostgreSQL has

`Fn` calls a function the package does not wrap. You supply the name, the
arguments and — through the type parameter — what it returns:

```go
// pg_size_pretty(pg_total_relation_size('users'))
size := orm.Fn[User, string]("pg_size_pretty",
    orm.ArgRaw("pg_total_relation_size('users')"))

// greatest(score, 0)
floor := orm.Fn[User, int32]("greatest", orm.ArgOf(Users.Score), orm.ArgValue(0))
```

| Form | Returns | For |
| --- | --- | --- |
| `Fn[E, T]` | `Value[E, T]` | a value in an entity query |
| `FnNull[E, T]` | `Value[E, *T]` | when it can be NULL |
| `FnExpr[T]` | `Expression[T, *T]` | a value in a composed query |
| `FnExprNull[T]` | `Expression[*T, *T]` | |
| `FnPredicate[E]` | `Predicate[E]` | a function returning boolean |

Arguments are built rather than formatted:

```go
orm.ArgValue(v)          // a bind parameter
orm.ArgOf(Users.Email)   // a column
orm.ArgOpt(Users.Bio)    // a nullable column
orm.ArgCast(v, "uuid")   // a parameter with an explicit cast
orm.ArgRaw("now()")      // SQL text, no values in it
```

The type parameter is a promise you are making about what the function returns,
and it is not checked against PostgreSQL. Get it wrong and the scan fails — this
is the escape hatch, and it is honest about being one.

## Raw fragments

When even `Fn` is the wrong shape:

```go
db.Users.Query().Where(orm.Expr[User]("age(created_at) > interval ?", "1 year"))
```

`Expr` takes SQL text deliberately. It does not take values formatted into it:
every `?` becomes a bind parameter, and the fragment's placeholders are counted
against the arguments given, so a mismatch is a build error rather than a
confusing server one.

## Worked examples

### A shipping band

`CASE` turning a number into a label, in SQL, so it can be grouped by:

```go
band := orm.Case(orm.Cond(Parcels.Grams.Lt(500)), orm.Val("letter")).
    When(orm.Cond(Parcels.Grams.Lt(2000)), orm.Val("small")).
    When(orm.Cond(Parcels.Grams.Lt(20000)), orm.Val("parcel")).
    Else(orm.Val("freight"))

var byBand = orm.Project2(
    band, orm.Count[orm.Composed](),
    func(b string, n int64) Band { return Band{b, n} },
)

orm.Compose(pool, byBand).From(Parcels.Source()).GroupBy(band).All(ctx)
```

Doing this in Go would mean fetching every parcel to count four numbers.

### A display name that is never empty

```go
name := orm.Coalesce(Members.Nickname, orm.Of(Members.Email))
```

`Nickname` is nullable, `Email` is not, so the result cannot be NULL and its type
says `string`. The fallback chain is the proof, not a convention.

### Treating blank as missing

```go
// An empty note is not a note.
note := orm.NullIf(orm.Of(Tickets.Note), orm.Val(""))
```

### Case-insensitive matching that uses an index

```go
// With an index on lower(email), this can use it; ILike cannot.
orm.Compose(pool, shape).From(Members.Source()).
    Where(orm.Eq(orm.Lower(Members.Email), orm.Lower(orm.Val(input))))
```

### Something PostgreSQL has and this package does not wrap

```go
// greatest(stock - reserved, 0)
available := orm.Fn[Item, int32]("greatest",
    orm.ArgOf(Items.Stock.SubCol(Items.Reserved)),
    orm.ArgValue(int32(0)))

// A function returning boolean, used as a predicate.
db.Items.Query().Where(orm.FnPredicate[Item]("pg_try_advisory_lock", orm.ArgOf(Items.ID)))
```

The type parameter is your promise about the return type. It is not checked
against PostgreSQL, which is what makes this the escape hatch rather than the
main road.
