# SQL composition

Up to M10 a query read one table. This is the part that reads several: a query
becomes a source, and sources compose — as derived tables, joins, CTEs and
correlated subqueries.

One idea underneath all of it:

> any typed query with a stable result shape is a typed SQL source, and nothing
> is lost on the way — not the result types, not the column names, not the
> identity of the occurrence, and not the NULL semantics.

## Contents

- [The composed query](#the-composed-query)
- [Lifting expressions: Of, Opt and Cond](#lifting-expressions-of-opt-and-cond)
- [Subqueries](#subqueries)
  - [EXISTS](#exists)
  - [IN and NOT IN](#in-and-not-in)
  - [Scalar subqueries](#scalar-subqueries)
- [Derived tables](#derived-tables)
- [UNION ALL](#union-all)
  - [What makes two branches compatible](#what-makes-two-branches-compatible)
  - [Where a set operation can go](#where-a-set-operation-can-go)
  - [Ordering](#ordering)
  - [What a branch may not carry](#what-a-branch-may-not-carry)
- [Joins](#joins)
  - [Outer joins make values nullable](#outer-joins-make-values-nullable)
  - [ON is not WHERE](#on-is-not-where)
  - [Scope is sequential](#scope-is-sequential)
  - [LATERAL](#lateral)
- [CTEs](#ctes)
  - [Recursive CTEs](#recursive-ctes)
  - [Data-modifying CTEs](#data-modifying-ctes)
- [Expressions](#expressions)
  - [CASE](#case)
  - [COALESCE and NULLIF](#coalesce-and-nullif)
  - [CAST](#cast)
  - [DISTINCT ON](#distinct-on)
  - [Strings, dates, tuples, arrays and JSON](#strings-dates-tuples-arrays-and-json)
- [Window functions](#window-functions)
- [`With` is not `Join`](#with-is-not-join)
- [What is checked where](#what-is-checked-where)

## The composed query

`orm.Compose` starts a query whose FROM clause is a list rather than a table:

```go
shape := orm.Project2(
    orm.Of(Users.ID), orm.Of(Users.Email),
    func(id int64, email string) Row { return Row{ID: id, Email: email} },
)

rows, err := orm.Compose(db.Executor(), shape).
    From(Users.Source()).
    Where(orm.Cond(Users.Active.Eq(true))).
    OrderBy(orm.Of(Users.ID).Asc()).
    All(ctx)
```

The result shape is an ordinary `Projection` — the same `ProjectN` constructors
an entity projection uses, instantiated at `orm.Composed` instead of at an
entity. Scanning is the same too: typed locals, one `Scan`, no reflection.

`orm.Rows` builds the other kind: a query meant to be a *source* rather than to
be read. Its select list is a list of declared outputs, which are also its
column names, so nothing has to be kept in step by hand.

```go
userID := orm.Named("user_id", orm.Of(Posts.AuthorID))
count  := orm.Named("post_count", orm.Of(orm.Count[orm.Composed]()))

stats := orm.Rows(userID, count).
    From(Posts.Source()).
    GroupBy(orm.Of(Posts.AuthorID))
```

Calling `All` on such a query is an error: it has no result shape, because
nothing scans it directly.

## Lifting expressions: Of, Opt and Cond

An entity query checks expressions by their entity tag: a `Predicate[User]`
cannot reach a query over `Post`. A composed query has no single entity, so the
checking moves to where it was always really happening — **source identity**. An
expression depends on the occurrences it was built from, the compiler knows
which occurrences a statement introduces and in what order, and one that is not
in scope is refused before PostgreSQL sees it.

Three functions cross the boundary:

| | what it does |
|---|---|
| `orm.Of(x)` | lifts an expression, keeping its result type |
| `orm.Opt(x)` | lifts it as its **nullable** form — for an outer-joined source |
| `orm.Cond(p)` | lifts a predicate, so every typed comparison still works |

```go
orm.Of(Users.Email)          // Expression[string, *string]
orm.Of(Users.Bio)            // Expression[*string, *string]
orm.Opt(Profiles.ID)         // Expression[*int64, *int64]
orm.Opt(Profiles.Bio)        // Expression[*string, *string]  — already nullable
orm.Cond(Users.Active.Eq(true))
```

`Opt` is idempotent on columns: a nullable column's nullable form is itself. For
an expression that is already nullable and is *not* a column — a `Min`, a scalar
subquery — use `orm.OfNull`, which says so.

Comparing across sources uses the free functions, which compare by **value type**
so a nullable foreign key can be compared to the non-nullable key it points at:

```go
orm.Eq(Posts.AuthorID, Users.ID)     // *int64 against int64
```

`Eq`, `Ne`, `Lt`, `Lte`, `Gt`, `Gte`.

## Subqueries

### EXISTS

```go
db.Users.Query().Where(orm.Exists[User](
    orm.Rows(orm.Named("x", orm.Val(1))).
        From(Posts.Source()).
        Where(orm.Eq(Posts.AuthorID, Users.ID)),
))
```

The subquery correlates to the enclosing query by naming its source. The scope
is a stack of frames, so this nests: a subquery of a subquery may name either of
the queries above it, and may not name anything else. There is a depth limit
(32) so that a pathological tree is refused rather than overflowing a stack.

`orm.NotExists` is the negation, and it is not `NOT IN`.

### IN and NOT IN

```go
authors := orm.Rows(orm.Named("author_id", orm.Of(Posts.AuthorID))).From(Posts.Source())

db.Users.Query().Where(orm.InSub(Users.ID, authors))
db.Users.Query().Where(orm.NotInSub(Users.ID, authors))
```

The subquery must return exactly one column, which is checked when the statement
is built.

> **NOT IN and NULL.** If the subquery returns a NULL, `NOT IN` is UNKNOWN for
> every row — a `WHERE` keeps rows only on TRUE — so the query returns *nothing*,
> even for values plainly not in the list. This is PostgreSQL's three-valued
> logic and it is **not** rewritten into `NOT EXISTS`, which would return
> different rows from the SQL you are reading. If the subquery can produce NULL
> and you meant "has no match", write `NotExists`, or exclude the NULLs inside
> the subquery.

### Scalar subqueries

```go
latest := orm.Scalar[User, time.Time](
    orm.Rows(orm.NamedNull("m", orm.Max(Posts.CreatedAt))).
        From(Posts.Source()).
        Where(orm.Eq(Posts.AuthorID, Users.ID)),
)   // Value[User, *time.Time]
```

> **A scalar subquery is always nullable.** PostgreSQL returns NULL when the
> statement matches no row, whatever the expression it selects — so a subquery
> over a `NOT NULL` column is nullable, and so is one selecting `count(*)`. The
> count cannot be NULL; a statement returning no row at all still yields one.

More than one row is a cardinality violation PostgreSQL raises at run time, and
it arrives as a `*pgconn.PgError` like any other server error.

## Derived tables

`orm.Sub` turns a row-producing query into a source. Its columns are read with
`orm.Ref`, typed by the declaration rather than by a string:

```go
stats := orm.Sub("post_stats", orm.Rows(userID, count).
    From(Posts.Source()).
    GroupBy(orm.Of(Posts.AuthorID)))

orm.Ref(stats, count)      // Expression[int64, *int64]
```

Naming a column the source does not provide is refused when the statement
compiles, with the columns it does have. Two outputs of one name are refused
when the source is built.

A derived source is a value: `stats.As("other")` is a second source, the query it
was built from is untouched, and one query definition can back several sources
in several statements. Nothing about a compilation is stored in it.

## UNION ALL

`orm.UnionAll` concatenates the rows of two or more branches, keeping
duplicates. The branches are variadic rather than a pair, because
`A UNION ALL B UNION ALL C` is one operation over three inputs and not a tree of
pairs somebody associated:

```go
active := orm.Compose(nil, summary).From(Users.Source()).Where(...)
archived := orm.Compose(nil, summary).From(Archived.Source()).Where(...)

orm.UnionAll(active, archived).All(ctx)
```

Anything this package builds that produces rows can be a branch: an entity query,
a projection query, a composed query, and another set operation.

Duplicates are kept. That is what ALL means, and it is the cheaper operation —
removing them costs a sort or a hash of the whole result.

### What makes two branches compatible

Two things, and the Go result type is only the first. The compiler enforces that
every branch produces the same `R`. The builder enforces that they produce the
same *result shape*: the number of columns, and each column's Go destination type
and nullability, positionally.

`R` alone would not be enough. These two projections build the same struct out of
different columns:

```go
orm.Project2(Users.Email, Users.Name, func(a, b string) Pair { ... })
orm.Project2(Users.Name, Users.Nick,  func(a, b string) Pair { ... })
```

so a rule reading only `R` would accept a branch feeding a nullable nickname into
a destination the other branch proved non-null.

A mismatch is refused rather than reconciled. `int32` against `int64`, `T`
against `*T`, nullable against non-nullable: PostgreSQL could coerce some of
those pairs, and coercing them here would mean choosing a destination type you
did not write. What is left to PostgreSQL is whether the two SQL expressions in a
column have compatible SQL types — it checks that when the statement runs and
reports it precisely.

Every branch is compared against the first, not against the one before it. The
first branch owns the result: its scanner reads every row, and PostgreSQL takes
the compound's output names from it.

### Where a set operation can go

A validated set operation is a value, and it goes wherever its shape allows:

```go
u := orm.UnionAll(fromPosts, fromArchive)

orm.Sub("recent", u)                    // a derived table
orm.CTE("recent", u)                    // an ordinary CTE body
orm.InSub(Users.ID, u)                  // a membership test
orm.Scalar[User, int64](u)              // a scalar value
orm.UnionAll(u, other)                  // a branch of another operation
```

Each of those asks for something different, and the difference is deliberate. A
source's columns are addressed by name, so `Sub` and `CTE` require the first
branch to name them — a compound's output names are the first branch's, which is
PostgreSQL's rule. A value subquery's columns are not addressed at all, so `InSub`
and `Scalar` require no names and check the arity instead: as many columns as the
expression on the left of an `IN`, and exactly one for a scalar. Both are decided
when the query is built.

Using one as a recursive CTE's anchor or inside `EXISTS` is not offered: both
rewrite or compare the select list they are given, which only a plain `SELECT`
can answer.

### Ordering

A compound's `ORDER BY` takes an output column name and nothing else — not a
qualified reference, and not an expression, even one over an output name. So the
terms name the operation's own result:

```go
thingID := orm.Named("thing_id", orm.Of(Users.ID))

orm.UnionAll(fromPosts, fromArchive).
    OrderBy(thingID.Desc()).
    Limit(10)
```

The declaration is the same value `orm.Ref` takes, so a union that is both
ordered and selected from names its columns once. A name the result does not have
is refused with the ones it does; a first branch that names none of its columns
has nothing to order by and says so.

The clause belongs to the operation rather than to its last branch, so `Limit`
after `OrderBy` returns the first rows rather than some rows.

### What a branch may not carry

A branch is a statement, and two things go wrong differently.

A clause the grammar attaches to the compound is parenthesised so it stays the
branch's — `ORDER BY`, `LIMIT`, `OFFSET`, and a `WITH` clause. That last one
matters most: written bare in the first branch, PostgreSQL accepts it and
declares the item for the *whole* operation, so the branch-local declaration you
wrote is not what runs.

A locking clause cannot be there at all. PostgreSQL refuses `FOR UPDATE`, and
every other strength, anywhere in a set operation — parenthesising does not help,
because a concatenation has no table rows to lock. It is refused when the branch
is handed over.

To share a named query between branches, declare it on the operation, which is
where PostgreSQL puts it:

```go
recent := orm.CTE("recent", ...)

orm.UnionAll(fromRecent, alsoFromRecent).With(recent)
```

A branch's own `With` belongs to that branch. One branch cannot read what another
declared.

## Joins

```go
orm.Compose(ex, shape).
    From(Users.Source()).
    Join(Posts.Source(), orm.Eq(Posts.AuthorID, Users.ID)).
    LeftJoin(Profiles.Source(), orm.Eq(Profiles.UserID, Users.ID))
```

`Join`, `LeftJoin`, `RightJoin`, `FullJoin`, `CrossJoin`, and the lateral forms
`JoinLateral`, `LeftJoinLateral`, `CrossJoinLateral`. The target is any source: a
table occurrence, an alias, a derived table, a CTE reference. There is one join
implementation because there is one kind of thing to join.

A `CROSS JOIN` takes no condition, and passing one is refused rather than
dropped.

### Outer joins make values nullable

This is the rule the whole design turns on.

```sql
SELECT p.name FROM users u LEFT JOIN profiles p ON ...
```

Even with `profiles.name NOT NULL`, `p.name` is nullable here: the join can
produce a row in which the entire right-hand source is absent. **Nullability is
not only a property of the column — it is a property of how its source enters
this query.**

So read outer-joined values with `Opt` (columns and expressions) or `OptRef`
(declared outputs of a derived table or CTE):

```go
orm.Compose(ex, orm.Project2(
    orm.Of(Users.ID),
    orm.Opt(Profiles.Bio),      // *string
    build,
)).
    From(Users.Source()).
    LeftJoin(Profiles.Source(), orm.Eq(Profiles.UserID, Users.ID))
```

A select list that does not is **refused**, naming the source and saying how to
fix it — rather than left to fail as a scan error on the first parent with no
child. The rule per join:

| join | what becomes nullable |
|---|---|
| `Join` (INNER), `CrossJoin` | nothing |
| `LeftJoin` | the source being attached |
| `RightJoin` | every source introduced before it |
| `FullJoin` | both |

Nullability propagates through computation, because SQL propagates NULL:
`orm.Opt(Posts.Score).Add(&one)` is nullable, and its type says so.

Two things are *not* affected. An aggregate is a barrier — `count(p.id)` over a
`LEFT JOIN` is zero rather than NULL, and its own result type is the answer. And
a `COALESCE` whose fallback cannot be NULL cannot be NULL either, so
`coalesce(s.post_count, 0)` is a non-nullable `int64`.

None of this touches the descriptors. `Profiles.ID` still means a `NOT NULL`
column in every other query; the widening belongs to this statement.

### ON is not WHERE

```sql
LEFT JOIN posts ON posts.user_id = users.id AND posts.published   -- keeps every user
LEFT JOIN posts ON posts.user_id = users.id WHERE posts.published -- drops users with no published post
```

These are different queries. A condition passed to a join goes in its `ON`
clause; a condition passed to `Where` goes in `WHERE`. Nothing is moved between
them.

### Scope is sequential

A join condition may name the sources introduced before it and the one it is
attaching — nothing further along:

```go
From(Users.Source()).
    Join(Posts.Source(), orm.Eq(Comments.ID, Posts.ID)).   // refused: comments comes later
    Join(Comments.Source(), ...)
```

The message is about the order, not about a column PostgreSQL could not resolve.

Two occurrences claiming one alias are refused too, which is what makes a self
join safe:

```go
employee := Users.As("employee")
manager  := Users.As("manager")

From(employee.Source()).
    LeftJoin(manager.Source(), orm.Eq(employee.ManagerID, manager.ID))
```

### LATERAL

A plain derived table is evaluated once, independently of the sources beside it,
so it cannot name one of them — that is refused, naming the derived table.
`LATERAL` is exactly the permission to do it, and even then only leftwards:

```go
latest := orm.Sub("p", orm.Rows(postID).
    From(Posts.Source()).
    Where(orm.Eq(Posts.AuthorID, Users.ID)).
    OrderBy(orm.Of(Posts.CreatedAt).Desc()).
    Limit(1))

orm.Compose(ex, shape).From(Users.Source()).LeftJoinLateral(latest)
```

With no condition the join is `ON TRUE`, which is what such a subquery almost
always wants — it has already said which rows it matches.

## CTEs

```go
active := orm.CTE("active_users", orm.Rows(id, email).
    From(Users.Source()).
    Where(orm.Cond(Users.Active.Eq(true))))

orm.Compose(ex, shape).With(active).From(active)
```

The value is both the declaration `With` renders and the reference `From` and
the joins take. `active.As("a")` is a second reference, which is how one CTE is
joined to itself.

Declaration order is sequential and checked: a later item may name an earlier
one, an earlier one may not name a later one, and only a recursive item may name
itself. Two items of one name are refused.

A CTE named after a table shadows it inside that statement, which is
PostgreSQL's rule — and the two never blur, because a table source writes its
schema-qualified name and a CTE reference writes the bare one.

`orm.Materialized` and `orm.NotMaterialized` are declaration options, not
transformations, so the source keeps one identity:

```go
orm.CTE("expensive", body, orm.Materialized)
```

### Recursive CTEs

```go
id        := orm.Named("id", orm.Of(Users.ID))
managerID := orm.Named("manager_id", orm.Of(Users.ManagerID))

tree := orm.RecursiveCTE("tree",
    orm.Rows(id, managerID).From(Users.Source()).Where(orm.Cond(Users.ID.Eq(root))),
    func(self *orm.Source) orm.Term {
        return orm.Rows(
            orm.Named("id", orm.Of(Users.ID)),
            orm.Named("manager_id", orm.Of(Users.ManagerID)),
        ).
            From(Users.Source()).
            Join(self, orm.Eq(Users.ManagerID, orm.Ref(self, id)))
    })
```

The builder hands the recursive term a source referring to the CTE itself,
because that source has to exist before the statement defining it is finished.

The output columns and their types come from the **anchor** term, and the
recursive term is checked against them: a different arity, a different set of
names, or a recursive term that can produce NULL where the anchor cannot are all
refused. PostgreSQL checks the column *types* itself, and precisely.

The terms are combined with `UNION ALL`. `orm.RecursiveCTEUnion` uses `UNION`
instead, which removes rows already produced — the thing that makes a query over
a cyclic graph terminate. Termination is otherwise yours: nothing here inspects
the recursive term for a stopping condition or counts iterations in Go.

### Data-modifying CTEs

```go
changed := orm.WritingCTE("changed", orm.UpdateReturning(update, shape))

orm.Compose(ex, report).With(changed).From(changed)
```

PostgreSQL runs such an item **exactly once**, whether or not the main query
reads its rows, and every part of the statement sees the same snapshot: rows the
item changed are visible to the rest of the statement only through the item's own
output. Every returned column has to be named, because a column of a CTE is
addressed by name.

## Expressions

### CASE

```go
orm.Case(orm.Cond(Users.Active.Eq(true)), orm.Val("active")).
    When(orm.Cond(Users.Age.Lt(int32(18))), orm.Val("minor")).
    Else(orm.Val("inactive"))    // Expression[string, *string]
```

The first branch decides the result type; every later branch and the `ELSE` must
produce the same one, so a mismatched branch does not compile.

`End()` finishes a CASE **without** an `ELSE`, and its result is nullable
whatever the branches produce — PostgreSQL's answer for a row matching no branch
is NULL. The two endings have different result types rather than a flag.

### COALESCE and NULLIF

```go
orm.Coalesce(orm.Val(""), orm.Of(Users.Nickname))   // Expression[string, *string]
orm.CoalesceNull(orm.Of(Users.Nickname), orm.Of(Users.Bio))
orm.NullIf(Users.Email, orm.Val(""))                // always nullable
```

`Coalesce` takes its fallback first in the signature and writes it last in the
SQL. Because the fallback cannot be NULL, the result cannot be — the one case
where a non-nullable COALESCE is provable rather than hoped for. `NullIf` is
always nullable, whatever its arguments are: producing NULL is what it is for.

### CAST

```go
orm.Cast(Users.ID, orm.Text)             // Expression[string, *string]
orm.CastNull(Users.Nickname, orm.Text)   // stays nullable
```

The target decides the Go result type, so a cast cannot relabel an expression as
whatever the caller wanted. `orm.Text`, `orm.BigInt`, `orm.Integer`,
`orm.SmallInt`, `orm.Boolean`, `orm.Real`, `orm.DoublePrecision`,
`orm.Timestamptz`, `orm.Timestamp`, `orm.Date`, `orm.ByteA` — and
`orm.PGTypeOf[V]("name")` for a type this package cannot state the mapping of,
which is the same trust boundary `RawValue` has.

> **Bare literals.** `orm.Val` is a bind parameter, so it carries no PostgreSQL
> type. Where the server cannot infer one from context — an argument of an
> overloaded function such as `lower`, or a select-list item standing alone — it
> refuses the statement rather than guessing. Say which type it is:
> `orm.Cast(orm.Val("ABC"), orm.Text)`.

### DISTINCT ON

```go
q.DistinctOn(orm.Of(Posts.AuthorID)).
    OrderBy(orm.Of(Posts.AuthorID).Asc(), orm.Of(Posts.CreatedAt).Desc())
```

PostgreSQL requires the leading `ORDER BY` terms to match the `DISTINCT ON`
expressions. That requirement is the server's to enforce: deciding whether two
expressions are the same one is a judgement about SQL equivalence this package
cannot make soundly, and an over-strict version would refuse queries PostgreSQL
runs happily.

`Distinct` and `DistinctOn` in one statement is refused — they are different
clauses.

### Strings, dates, tuples, arrays and JSON

A focused set, each with a `…Null` counterpart where the nullable input needs
one:

- `Lower`, `Upper`, `Trim`, `Length`, `Concat`
- `Now`, `DateTrunc`, `Extract`
- `Row2Eq`, `Row2In` with `orm.Both` — composite keys as one tuple comparison,
  which is what an index on those columns answers directly
- `AnyOf`, `AllOf`, `ArrayContains`, `ArrayContainedBy`, `ArrayOverlaps`
- `JSONText` (`->>`), `JSONHasKey` (`?`), `JSONContains` (`@>`)

`Extract` takes a type from the registry and writes the cast into the SQL,
because PostgreSQL's `extract` returns `numeric` — whose Go mapping is a
project's own decision, so claiming one here would be claiming something this
package cannot know. The `->` operator is deliberately absent for the same
reason: its result type and nullability cannot be stated exactly.

Everything else is `RawValue` and `Expr`, which stay the escape hatches.

## Window functions

```go
orm.RowNumber().Over(orm.Window().
    PartitionBy(orm.Of(Posts.AuthorID)).
    OrderBy(orm.Of(Posts.CreatedAt).Desc()))
```

`RowNumber`, `Rank`, `DenseRank` (bigint, never NULL), `PercentRank`,
`CumeDist`, `Ntile`, `Lag`, `LagN`, `Lead`, `LeadN`, `FirstValue`, `LastValue`,
`NthValue`. Any aggregate computes over a window with `.Over(...)`:

```go
orm.SumInt32(Orders.Amount).Over(orm.Window().PartitionBy(orm.Of(Orders.UserID)))
```

A windowed aggregate keeps its own result type — a windowed `sum` can still be
NULL, a windowed `count` still cannot — and it no longer implies a `GROUP BY`.
Nothing here adds one.

> **`Lag` and `Lead` are nullable.** The row they read may not exist — the first
> row of every partition has nothing before it — so the result is NULL whatever
> the expression's own nullability is.

Frames: `.Rows(start, end)`, `.Range(...)`, `.Groups(...)` with
`UnboundedPreceding()`, `Preceding(n)`, `CurrentRow()`, `Following(n)`,
`UnboundedFollowing()`. A frame that starts at `UNBOUNDED FOLLOWING`, ends at
`UNBOUNDED PRECEDING`, starts after it ends, or carries a negative offset is
refused before PostgreSQL sees it.

A window's `ORDER BY` is the window's. It never joins the statement's.

### Filtering on a window

PostgreSQL computes windows *after* `WHERE`, `GROUP BY` and `HAVING` have
decided which rows exist, so a window function is refused in all of them — and in
a join condition and `DISTINCT ON`. Filtering on one means wrapping the
statement, which is what a derived table is for:

```go
rn := orm.Named("rn", orm.RowNumber().Over(byAuthor()))
ranked := orm.Sub("p", orm.Rows(postID, author, rn).From(Posts.Source()))

orm.Compose(ex, shape).From(ranked).Where(orm.Ref(ranked, rn).Eq(int64(1)))
```

There is no `QUALIFY`: PostgreSQL has no such clause, and inventing one would
mean rewriting your query into a shape you did not write.

## `With` is not `Join`

Two different features with two different jobs, and they stay apart:

- **`Query.With`** loads *relations*. It is entity loading: you get entities with
  their related entities attached, the cardinality of the root rows is untouched,
  and the strategy — folded join, batched second statement, LATERAL — is the
  planner's.
- **`ComposedQuery.Join`** composes *SQL*. It is query composition: you get the
  rows the join produces, in the result shape you asked for.

Asking for a relation never changes which root rows come back. Writing a join
does, because that is what a join is. `With` is not implemented in terms of
public joins and public joins do not hydrate entities.

## What is checked where

Being exact about this is the point of the design, so here is the boundary.

**The Go compiler refuses:**

- a derived or CTE column read back at a type its declaration does not produce
- an outer-joined column read into a destination that cannot hold NULL
  (via `Opt`/`OptRef` — the widened type is what is bound)
- a scalar subquery, `NullIf`, `Lag`, `Lead`, or an ELSE-less `CASE` bound to a
  non-nullable result
- a `CASE` branch of another type, a `COALESCE` fallback of another type, a
  string function over a non-text column, an array operator over mismatched
  element types
- a window function used before it has a window
- an entity's expression or predicate handed to a composed query, or the reverse

**The builder refuses, before PostgreSQL is asked:**

- a select list that reads an outer-joined value into a non-nullable result
- a join condition naming a source joined later
- a derived table correlating to a sibling without `LATERAL`, and a lateral one
  naming a source introduced after it
- a column a derived table or CTE does not provide
- two sources claiming one alias, or one source introduced twice in one FROM
  clause; two WITH items of one name; two outputs of one name
- `LATERAL` attached to anything but a subquery
- a WITH item naming one declared after it, or naming itself without being
  recursive
- a recursive term whose arity, column names or nullability disagree with the
  anchor
- an aggregate in `WHERE`, `GROUP BY`, `DISTINCT ON` or a join condition
- a window function in `WHERE`, `HAVING`, `GROUP BY`, `DISTINCT ON` or a join
  condition
- an impossible frame; a multi-column subquery in `IN` or a scalar position
- `DISTINCT` and `DISTINCT ON` together; statements nested more than 32 deep

**PostgreSQL decides, and its error is preserved as a `*pgconn.PgError`:**

- whether the column types of a recursive CTE's two terms are compatible
- whether a select list is consistent with its `GROUP BY`
- whether `DISTINCT ON` agrees with the `ORDER BY`
- a scalar subquery returning more than one row
- every type mismatch a `RawValue`, `Expr` or `Raw` fragment introduced

## Bulk ingestion: COPY

`InsertMany` sends a statement — one placeholder per value, RETURNING for what
the database generated, ON CONFLICT for what to do about clashes. `CopyFrom`
sends rows over PostgreSQL's copy protocol instead: no statement to parse, no
per-row round trip, and none of those three things.

```go
n, err := db.Events.CopyFrom(ctx, events)

n, err := db.Events.CopyFromSeq(ctx, func(yield func(Event, error) bool) {
    for scanner.Scan() {
        if !yield(parse(scanner.Text())) {
            return
        }
    }
})

n, err := orm.CopyColumns(ctx, db.Events, events, Events.UserID, Events.Kind)
```

The sequence form pulls one row at a time, so a file larger than memory is a
file this can ingest.

**Which to use.** COPY is for ingestion — loading a file, backfilling a table,
importing a feed. `InsertMany` is for writing rows your program then uses,
because it can hand back the identities PostgreSQL assigned and decide what
happens to a row that conflicts. COPY is faster; it is not universally better.

**Columns.** The whole-entity form copies every writable column; generated and
identity columns are never mentioned, so PostgreSQL supplies them. A column
`CopyColumns` leaves out takes the table's DEFAULT, its identity, or NULL —
nothing substitutes a Go zero for it. A zero you did copy is a value.

**Transactions.** A repository bound to a transaction copies inside it, so a
COPY and the statements around it roll back together. On its own, a COPY is one
statement: it either applies entirely or not at all, and that is the whole of
the guarantee.

**Errors.** PostgreSQL reports a copy failure against the stream rather than
against a row, so the error names the table and carries the server's own
`*pgconn.PgError` — and no row index, because there is none to report.

## PostgreSQL-native types and operators

**Network.** `inet` and `cidr` map to `netip.Prefix`, `macaddr` to
`net.HardwareAddr` — an address with a prefix length, which is what PostgreSQL
stores. `ContainedBy`, `ContainsNetwork`, `NetworksOverlap` and friends are
`<<`, `>>` and `&&`.

**JSONB.** `JSONGet`, `JSONText`, `JSONPathGet`, `JSONPathText`, containment,
key existence, `JSONSet`, `JSONInsert`, and the JSONPath predicates. Paths and
keys are bind parameters, never assembled SQL.

> **Three kinds of nothing.** A SQL NULL column, a JSON null value and a missing
> key are different, and the operators keep them apart: `->>` is NULL for the
> first and third and the JSON null for the second, while `?` is TRUE for a key
> whose value is JSON null. Do not collapse them in Go.

**Full text search.** `tsvector` and `tsquery` are `orm.TSVector` and
`orm.TSQuery`, not strings — a parsed document is not a bag of words. Build with
`ToTSVector` and one of the four query constructors under a named
`TextSearchConfig`, match with `Matches`, rank with `TSRank` — or `TSRankNull` when the vector can
be NULL, which a generated column and an outer-joined one both can. Declare the vector
as a generated column and index it with GIN; nothing here creates an index for
you.

**Row locking.**

```go
db.Jobs.Query().
    Where(Jobs.Status.Eq(Pending)).
    OrderBy(Jobs.ID.Asc()).
    Limit(10).
    Lock(orm.ForUpdateStrong, orm.SkipLocked)
```

A lock is held until the transaction ends, so these are only meaningful inside
one. Pick the weakest strength that answers your question. `SkipLocked` is how
workers claim disjoint rows — it makes no promise about how many come back.
`NoWait` fails rather than queueing. Neither is a general synchronisation
primitive.
