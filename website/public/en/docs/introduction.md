# Introduction

> What this ORM is, what it refuses to do, and why the difference matters.

Source: https://ormgo.vercel.app/en/docs/introduction/
Symbols: https://ormgo.vercel.app/api/orm.txt — the generated list of every exported name.

---
## The thesis

> You own your structs. PostgreSQL owns your schema. The generator proves they agree.

Most Go data mappers pick a side. Either the structs are the source of truth and the schema is generated from them, or the schema is the source of truth and the structs are generated from it. Both directions produce code somebody has to keep in step by hand the moment reality diverges.

This one generates neither from the other. You write the entity structs. Migrations own the schema. The `orm` command introspects both, reports every place they disagree, and generates typed metadata **only from a mapping it proved**.

The consequence is the point: a query that compiles is a query the database can answer.

## What that buys

The generated descriptors carry the type parameters, so the compiler enforces what reconciliation proved:

```go
// A Predicate[User] cannot reach a query over Post.
db.Orders.Query().Where(Users.Email.Eq("a@example.com")) // does not compile

// A text column has Like. An integer does not.
Users.Email.ILike("%@example.com") // fine
Users.Age.ILike("%")               // does not compile

// A NOT NULL column has no IsNull.
Users.Bio.IsNull()  // fine: bio is nullable
Users.Email.IsNull() // does not compile
```

None of that is a naming convention or a lint rule. It falls out of the descriptor's type, which came from the catalog.

## What it refuses to do

A tool is defined as much by what it will not do:

- **No lazy loading.** A relation is loaded because you asked for it. A loop over a slice cannot silently become a query per row.
- **No dirty tracking, no identity map, no `Save`.** `Insert`, `Update` and `Delete` state intent. Nothing is inferred.
- **No implicit zero-value handling.** A struct with `Active: false` stores `false`, because the library cannot tell that from a field somebody left alone. Asking for the column default is [`orm.Default`](/en/docs/writing/), which is explicit.
- **No SQL string building.** Every value a caller passes becomes a bind parameter. `Expr` and `Raw` accept SQL text deliberately; neither accepts values formatted into it.
- **No `WHERE`-less updates by accident.** An update or delete with no conditions is refused with `ErrMissingWhere` unless `All` says every row was meant.

## Where the guarantees come from

Three layers, each with a different job.

| Layer | Owns | Proves |
| --- | --- | --- |
| Migrations | The schema | That the database can be rebuilt from nothing |
| Reconciliation | The mapping | That every struct field has a column, with a compatible type |
| Generated code | The descriptors | That a wrong query does not compile |

If reconciliation cannot prove a field, it does not generate a descriptor for it — it reports an error naming the field, the column and the fix. There is no fallback to `any`.

## Supported PostgreSQL

14, 15, 16, 17 and 18. Not "should work on"; the compatibility suite refuses to run against fewer than all five, because a version listed as supported and never run against is a claim nobody should believe.

## Where to go next

- [Installation](/en/docs/installation/) — add the module and the CLI.
- [Quickstart](/en/docs/quickstart/) — a schema, a struct and a query in a few minutes.
- [Core concepts](/en/docs/concepts/) — the vocabulary the rest of the docs uses.

## Worked examples

A taste of what the compiler is doing for you, in four unrelated schemas.

```go
// A shop. Text has Like; the compiler knows because the catalog said text.
db.Products.Query().Where(Products.Name.ILike("%lamp%"))

// A ledger. The balance check is in the WHERE, so an overdraft is an update
// that matched nothing rather than a race.
db.Accounts.Update().
    Set(Accounts.Balance.SetExpr(Accounts.Balance.Sub(amount))).
    Where(Accounts.ID.Eq(id)).
    Where(Accounts.Balance.Gte(amount)).
    Exec(ctx)

// A calendar. Overlap is one predicate, not four comparisons to get right.
db.Bookings.Query().Where(Bookings.During.Overlaps(orm.ClosedOpen(from, to)))

// A fleet. Three levels loaded in three statements, whatever the row count.
db.Depots.Query().With(Depots.Vehicles.With(Vehicles.Services)).All(ctx)
```

And four things that do not compile, which is the same claim from the other side:

```go
Products.PriceCents.ILike("%")          // an integer has no ILike
Products.Name.IsNull()                  // name is NOT NULL
db.Accounts.Query().Where(Products.Name.Eq("x"))  // wrong entity
orm.UnionAll[Row](byEmail, byAge)       // branches disagree on shape
```
