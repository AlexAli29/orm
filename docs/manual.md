# orm — the manual

> You own your structs. PostgreSQL owns your schema. The generator proves they agree.

Most Go data-access tools pick a side. An ORM generates the schema from the
structs and turns your database into an implementation detail. A query compiler
generates the structs from the schema and turns your domain model into a
transcript of `information_schema`. Both work by making one representation
authoritative and the other derived.

This project does neither. You write the structs, and something authoritative
owns the schema — either your existing database or a set of reviewed migration
files. `orm check` introspects both and reports every place they disagree.

Once the mapping is proven, `orm generate` writes typed metadata from it, and
that metadata is what makes queries type-safe without reflection.

A PostgreSQL-native data mapper for Go: it reconciles user-owned structs against
the real database schema and generates a type-safe dynamic query API from what it
proved.

Requires Go 1.24 and PostgreSQL 14 or newer. The only runtime dependency is
[pgx](https://github.com/jackc/pgx).

## What it is not

Stating this early saves everyone time:

- **PostgreSQL only.** Not a portable abstraction over four databases. The
  features that make it worth using — `WITH ORDINALITY`, `LATERAL`, correlated
  `EXISTS`, catalog introspection — are PostgreSQL's.
- **Nothing happens to your schema implicitly.** There is no `AUTO MIGRATE` and
  no DDL at run time. In the default mode PostgreSQL owns the schema and you use
  whatever migration tool you already have. If you would rather the Go
  declarations owned it, [managed mode](migrations.md) writes migration
  files you read and apply on purpose — `makemigrations`, then `migrate`, with
  the SQL printable in between.
- **No lazy loading.** A relation you did not ask for stays unloaded. Nothing
  fetches itself behind a field access, so a loop cannot become N queries.
- **No identity map, no dirty tracking, no `Save`.** Write intent is stated:
  `Insert`, `Update`, `Delete`. Nothing infers what changed.
- **No hidden zero-value semantics.** `Active: false` stores `false`. Asking for
  the column's default is a separate, explicit call.
- **No cascades, no lifecycle hooks, no second-level cache.**

### Why not sqlc?

sqlc is excellent when your queries are written as SQL — you get exactly the
statement you wrote and types generated from it. This project is for the case
sqlc handles least well: a query whose *shape* is decided at run time, where the
set of conditions comes from a request and every combination has to produce
valid SQL. The two are complementary, and using both in one codebase is
reasonable.

### Why not GORM?

No runtime model inference, no schema ownership, no `Save` deciding between
insert and update, no lazy loading, no silent omission of zero values. Agreement
between structs and schema is proved once, at generate time, by reading
`pg_catalog` — not assumed at run time from struct tags.

## Dynamic filters, checked at compile time

This is the case the design is built around. A filter set assembled at run time,
where every combination — including none at all — has to produce valid SQL:

```go
predicates := []orm.Predicate[User]{}

if filter.Email != "" {
    predicates = append(predicates, Users.Email.ILike("%"+filter.Email+"%"))
}
if filter.HasPublishedPosts {
    predicates = append(predicates, Users.Posts.Any(Posts.Published.Eq(true)))
}

users, err := db.Users.
    Query().
    Where(orm.And(predicates...)).
    With(
        Users.Posts.
            Where(Posts.Published.Eq(true)).
            OrderBy(Posts.CreatedAt.Desc()).
            Limit(5).
            With(
                Posts.Comments.
                    OrderBy(Comments.CreatedAt.Desc()).
                    Limit(3),
            ),
    ).
    OrderBy(Users.CreatedAt.Desc()).
    Limit(50).
    All(ctx)
```

Fifty users at most, each with their five most recent published posts, each of
those with its three most recent comments — in **three statements**, whatever the
row counts. A runnable version of this is in [examples/blog](examples/blog).

With no filters set, that runs `SELECT ... FROM "public"."users" ORDER BY ...` —
no `WHERE` clause at all, rather than `WHERE ()` or a stray `AND`. `And` of
nothing is TRUE, which restricts nothing; `Or` of nothing is FALSE, which is the
honest answer to matching one of no alternatives. `In()` over an empty slice is
FALSE too, because PostgreSQL has no syntax for an empty `IN` list.

None of the following compiles, and none of them needs a database to be caught:

```go
Users.Age.Eq("18")                        // wrong value type
Users.Email.IsNull()                      // email is NOT NULL: there is no IsNull
Users.Age.ILike("%18%")                   // an integer has no pattern match
Posts.Published.Gt(true)                  // a bool has no magnitude
Users.ID.EqCol(Users.Email)               // int64 against text
Users.ID.EqCol(Posts.ID)                  // two entities
Users.Age.Set("18")                       // wrong assignment type
Users.Email.SetNull()                     // email is NOT NULL
orm.Default(Users.CreatedAt, Posts.CreatedAt)      // two entities
orm.OnConflict(Users.Email, Posts.ID)              // two entities
orm.And(Users.Active.Eq(true),
        Posts.Published.Eq(true))         // two entities in one predicate
db.Users.Query().Where(Posts.Published.Eq(true))   // the wrong entity's predicate
db.Users.Query().OrderBy(Posts.CreatedAt.Desc())   // the wrong entity's column
```

The query can always show its work, which is the difference between a builder
you can debug and one you cannot:

```go
sql, args, err := query.SQL()
// SELECT "users"."id", "users"."email", ... FROM "public"."users"
// WHERE "users"."email" ILIKE $1 AND "users"."age" >= $2
// ORDER BY "users"."created_at" DESC LIMIT 50
// args: ["%alex%", 18]
```

Every value is a parameter. Nothing a caller passes becomes SQL text.

## Try it

```console
$ orm init
wrote orm.yaml

$ orm check
reconciliation clean: the entities and the schema agree

$ orm generate
wrote internal/domain/orm_tables.gen.go
wrote internal/domain/orm_meta.gen.go
wrote internal/domain/orm_db.gen.go
```

`orm generate` runs the same reconciliation `orm check` does and refuses to
write anything if it does not hold. Code generated from a mapping that was never
proven would compile against a schema it disagrees with, which is the failure
this project exists to prevent.

When they do not agree:

```console
$ orm check
internal/domain/user.go:12:2: error: E004: nullable column public.users.nickname cannot be represented by string
    Go:          domain.User.Nickname string
    PostgreSQL:  public.users.nickname text
    reason:      string has no value that means SQL NULL
    fix:         use *string, or sql.Null[string]

internal/domain/user.go:5:1: warning: W003: column public.users.created_at is not mapped
    Go:          domain.User
    PostgreSQL:  public.users.created_at timestamptz
    reason:      the column has a default, so inserts succeed without it, but its value cannot be read back
    fix:         add a field for created_at to domain.User, or leave it unmapped on purpose and set strict.unmapped_columns: off

2 findings: 1 error, 1 warning
```

`--format json` produces a stable machine-readable document; `--format github`
produces GitHub Actions annotations. All three are byte-identical across runs,
carry no timestamps, and contain no absolute paths.

## Or let the declarations own the schema

The same tool runs the other way round. `schema.mode: managed` makes the Go
declarations the source of truth and writes migrations that carry them to
PostgreSQL — a state-based workflow inspired by Django's `makemigrations` /
`migrate` ergonomics, in PostgreSQL's own vocabulary rather than a portable one.

```console
$ orm init --mode managed
wrote orm.yaml

$ orm makemigrations --name initial
Migration 0001_initial

users
  + create table
      id int8 NOT NULL GENERATED BY DEFAULT AS IDENTITY
      email text NOT NULL
      status public.user_status NOT NULL DEFAULT 'pending'
      primary key users_pkey (id)
      unique constraint users_email_key (email)

wrote migrations/0001_initial.json

$ orm sqlmigrate 0001_initial      # read the SQL before it runs
$ orm migrate --plan               # read-only
$ orm migrate
Applying 0001_initial ... OK

$ orm check
Models       match the latest migration
Migrations   1 applied, none pending
Database     fully migrated, no drift
Mapping      valid
Generated    current

$ orm generate
```

Declarations say only what a Go type cannot: which column is the primary key,
what a default is, which index serves which query.

```go
//orm:table posts
//orm:index posts_feed_idx (AuthorID, CreatedAt desc) include (Title) where "status = 'published'"
//orm:check posts_title_not_blank "title <> ''"
type Post struct {
	ID        int64     `orm:"pk,identity"`
	AuthorID  int64
	Title     string
	Status    PostStatus `orm:"default:'draft'"`
	CreatedAt time.Time  `orm:"default:now()"`

	Author orm.One[User] `orm:"side:local"`
}
```

A rename is asked about rather than guessed:

```
Did you rename users.email to email_address?
    both are text, NOT NULL, with the same default. [y/N]
```

An existing database joins without being recreated: `orm makemigrations
--baseline` writes the migration describing what you already have — the one
migration command a database-first project may run, because it changes nothing —
and after switching the mode, `orm migrate --fake 0001_initial` records it,
verifying the claim first and running no schema SQL. Database-first projects are
unaffected: they have no migrations to report on and do not start failing
because they have none.

The whole thing is documented in **[docs/migrations.md](migrations.md)**,
and [examples/managed](examples/managed) is a project you can run.

## Entities

An entity is a struct that says which table it is:

```go
//orm:table users
type User struct {
    ID        int64
    Email     string `orm:"column:email_address"`
    Nickname  *string
    State     UserState
    CreatedAt time.Time

    Posts orm.Many[Post]
}

//orm:table posts
type Post struct {
    ID       int64
    AuthorID int64
    Title    string

    Author orm.One[User]
}
```

Nothing is inferred from the type name. `User` does not become `users` by
convention — it becomes `users` because the directive says so. A qualified name
works too: `//orm:table analytics.events`.

`orm.One` and `orm.Many` declare *how many* related rows there are. They do not
declare direction: which table holds the foreign key is a fact about the schema,
and the reconciler reads it out of `pg_constraint`. You only pin a relation when
the catalog is genuinely ambiguous:

```go
Author orm.One[User]  `orm:"fk:posts_author_fkey"`
Manager orm.One[User] `orm:"fk:users_manager_fkey,side:local"`
```

## Generated code

`orm generate` writes three files into each entity package, and a fourth when
the package declares a relation:

```
internal/domain/
├── entities.go          yours
├── orm_tables.gen.go    the table descriptors, their typed columns and relations
├── orm_meta.gen.go      the entity metadata and its scanner
├── orm_rel.gen.go       the relation loaders (only if there are relations)
└── orm_db.gen.go        the DB struct that binds repositories to an executor
```

Public names come from the table, private ones from the entity: table `users`
gives `Users` and `DB.Users`, entity `User` gives `userTable`, `userMeta` and
`userDest`. Nothing is renamed to avoid a clash — a generated identifier that
collides with one you wrote is reported, because silently becoming `Users2`
would leave you reading documentation about an identifier that does not exist.

Which descriptor a column gets is decided by its **PostgreSQL** type, never by
whatever Go type sits opposite it:

| PostgreSQL | Go field | Descriptor | Gains |
|---|---|---|---|
| `text NOT NULL` | `string` | `TextCol[User]` | `Like`, `ILike` |
| `text NULL` | `*string` | `NullTextCol[User]` | + `IsNull` |
| `int8 NOT NULL` | `int64` | `OrdCol[User, int64]` | `Gt`, `Between` |
| `timestamptz NOT NULL` | `time.Time` | `OrdCol[User, time.Time]` | `Gt`, `Between` |
| an enum | a named string type | `OrdCol[User, UserState]` | `Gt` — enums order, but do not `LIKE` |
| `bool NOT NULL` | `bool` | `Col[User, bool]` | equality only |
| `jsonb NOT NULL` | `map[string]any` | `Col[User, map[string]any]` | equality only |

`jsonb` does not acquire `Gt` because some Go representation of it happens to be
comparable, and `bytea` does not either: PostgreSQL defines an order for both,
but neither answers a question anybody asks. Nullable columns compare against a
*value*, not a pointer — a `*string` field yields `Eq(string)`, because SQL
compares values and NULL is tested for rather than compared to.

The scanner is a switch over a constant index, so reading a row uses no
reflection:

```go
func userDest(e *User, idx int) any {
	switch idx {
	case 0:
		return &e.ID
	case 1:
		return &e.Email
	default:
		return nil
	}
}
```

Generation is atomic and deterministic. All files are rendered and formatted
before any is written, each lands through a temporary file and a rename, and two
runs over the same structs and schema produce identical bytes — no timestamps,
no version stamps, no unsorted maps.

## The query API

Everything the read layer exposes:

```go
// Descriptors, on every column
Eq(V) · Ne(V) · In(...V) · EqCol(ColumnOf[E, V]) · Asc() · Desc()
// Ordered columns, additionally
Gt(V) · Gte(V) · Lt(V) · Lte(V) · Between(lo, hi V)
// Text columns, additionally
Like(string) · ILike(string)
// Nullable columns, additionally
IsNull() · IsNotNull()

// Combining
orm.And[E](...Predicate[E]) · orm.Or[E](...) · orm.Not[E](Predicate[E])
orm.Expr[E](sql string, args ...any)          // the escape hatch

// Building
db.Users.Query() *Query[User]
db.Users.QueryFrom(*orm.Source) *Query[User]  // select through an alias
    .Where(...Predicate[User]) *Query[User]   // and, across calls too
    .OrderBy(...Order[User]) *Query[User]
    .Limit(int) · .Offset(int) · .ForUpdate() *Query[User]
    .Clone() *Query[User]

// Running
    .SQL() (string, []any, error)
    .All(ctx) ([]User, error)
    .One(ctx) (User, error)
    .Count(ctx) (int64, error)
    .Exists(ctx) (bool, error)
    .Rows(ctx) iter.Seq2[User, error]
```

`Where` combines with AND, whether you pass several predicates, call it several
times, or wrap them in `orm.And` — the three are the same query.

Builders mutate and return the receiver, so a chain reads as one statement. A
builder method never panics and never returns an error: every mistake is
recorded, they all surface together from a terminal operation, and when any has
been recorded **no terminal operation touches PostgreSQL**.

A `Query` is not safe for concurrent use, and reusing one as a base needs
`Clone`:

```go
base := db.Users.Query().Where(Users.Active.Eq(true))

admins := base.Clone().Where(Users.Role.Eq(RoleAdmin))
recent := base.Clone().Where(Users.CreatedAt.Gte(cutoff))
```

Without the clones, `admins` and `recent` would accumulate onto `base` and onto
each other.

`Executor` is one method, so `*pgxpool.Pool`, `pgx.Tx` and `*pgx.Conn` all
satisfy it as they are:

```go
pool, err := pgxpool.New(ctx, dsn)
db := domain.New(pool)
```

## One row

```go
user, err := db.Users.
    Query().
    Where(Users.Email.Eq(email)).
    One(ctx)

switch {
case errors.Is(err, orm.ErrNotFound):
    // nothing matched
case errors.Is(err, orm.ErrMultipleRows):
    // more than one did
case err != nil:
    return err
}
```

`ErrMultipleRows` is not pedantry. A query that matches two rows is one whose
author believed something about the data that is not true, and quietly returning
the first hides that until it causes damage somewhere else. At most two rows are
fetched — enough to tell the three cases apart — and that limit is applied to a
copy, so the builder you hold is untouched.

## Counting and existence

```go
count, err := db.Users.Query().Where(Users.Active.Eq(true)).Count(ctx)
exists, err := db.Users.Query().Where(Users.Email.Eq(email)).Exists(ctx)
```

`Count` answers *how many rows would `All` return*: it respects `Where`, `Limit`
and `Offset`, and ignores ordering. It wraps the query rather than replacing its
column list, because a bare `count(*)` with a `LIMIT` would count the limit away
and report the whole table:

```sql
SELECT count(*) FROM (SELECT 1 FROM "public"."users" WHERE ... LIMIT 50) AS "_orm_count"
```

Neither materialises an entity — the server sends one number or one boolean.

## Pagination

```go
users, err := db.Users.
    Query().
    OrderBy(Users.CreatedAt.Desc()).
    Limit(50).
    Offset(100).
    All(ctx)
```

Pair `Offset` with an ordering. Without one, which rows get skipped is whatever
the server happened to produce first.

## Streaming

```go
for user, err := range db.Users.
    Query().
    Where(Users.Active.Eq(true)).
    Rows(ctx) {

    if err != nil {
        return err
    }
    process(user)
}
```

Nothing is buffered: each row is scanned as the loop reaches it, which is what
makes this usable over a result too large to hold. Stopping early closes the
rows and releases the connection, so `break` is safe. Errors arrive through the
loop rather than a second return.

## Locking

```go
users, err := db.Users.Query().Where(Users.ID.Eq(id)).ForUpdate().All(ctx)
```

Outside a transaction the lock is released immediately and buys nothing, so this
belongs inside one.

## The escape hatch

PostgreSQL is larger than any typed API, and a builder that cannot express what
the database can is one people abandon. `orm.Expr` takes SQL:

```go
users, err := db.Users.
    Query().
    Where(
        Users.Active.Eq(true),
        orm.Expr[User]("score > $1", 100),
    ).
    All(ctx)
```

```sql
WHERE "users"."active" = $1 AND score > $2
```

What you give up is the type checking — the fragment is not checked against the
entity's columns — and **nothing else**. The fragment's own placeholders are
renumbered into the surrounding statement and its arguments join the parameter
list, so values are still never part of the SQL text. Reach for a typed
predicate first; this is for what they cannot say.

Placeholders are checked when the predicate is built. A fragment referring to
`$2` with one argument, or handed an argument it never refers to, carries
`orm.ErrRawPlaceholder` and fails the query before PostgreSQL is touched. A
local `$1` used twice binds one argument, as it would in a statement written by
hand.

Finding the placeholders is a lexer, not a search-and-replace: `$1` inside a
string, a quoted identifier, a dollar-quoted body, or a line or nested block
comment is not a parameter, and is left alone.

## Aliases and scope

`As` returns descriptors for a second occurrence of the same table:

```go
manager := Users.As("manager")

manager.Email     // qualifies against "manager"."email"
Users.Email       // still "users"."email"
```

A source's identity is the value, not the name, so two aliases of one table are
two occurrences. Every column reference is checked against what the query
actually selects from:

```go
db.Users.Query().Where(manager.Email.Eq(addr))   // error
```

```
scope error: column "manager"."email" is not available in this query

the query selects from:
  public.users
```

Query through an alias with `QueryFrom`:

```go
db.Users.QueryFrom(manager.Source()).Where(manager.Email.Eq(addr))
```

Aliases beginning with `_` are reserved for the compiler's own, and an empty one
is refused. `EqCol` compares two columns rather than a column and a value, which
is what an alias exists for:

```go
manager.ID.EqCol(Users.ManagerID)
```

Both sides must be the same entity and the same value type, which the compiler
enforces.

## SQL inspection

Every query can show its work before it runs:

```go
sql, args, err := query.SQL()
```

`SQL` is the canonical build path — every terminal operation compiles through
it, so a query that fails there fails everywhere, and one that succeeds runs
exactly the statement it returned.

## Projections, aggregates and richer writes

An entity query returns entities. A projection query returns whatever you
select, in a Go shape you bind at compile time:

```go
type UserSummary struct {
	ID    int64
	Email string
}

var UserSummaries = orm.Project2(
	Users.ID, Users.Email,
	func(id int64, email string) UserSummary { return UserSummary{ID: id, Email: email} },
)

rows, err := orm.Select(db.Users, UserSummaries).
	Where(Users.Active.Eq(true)).
	OrderBy(Users.CreatedAt.Desc()).
	Limit(50).
	All(ctx)
```

`ProjectN` takes N expressions and a function of exactly their N result types.
Go has no type-parameter pack, so the arity is written out rather than faked
with `[]any` — which means the compiler checks the whole chain. A column of the
wrong type does not compile, and neither does a **nullable** column bound to a
destination that cannot hold NULL: `Users.DeletedAt` selects as `*time.Time`,
so SQL NULL can never arrive as a zero value.

Scanning uses no reflection. Each query gets its own typed locals, so a
two-column projection costs 36 allocations per thousand rows against the entity
scanner's 1049.

### Aggregates

```go
type AuthorPosts struct {
	AuthorID *int64
	Posts    int64
}

stats, err := orm.Select(db.Posts, orm.Project2(
	Posts.AuthorID, orm.Count[Post]().As("post_count"),
	func(id *int64, n int64) AuthorPosts { return AuthorPosts{id, n} },
)).
	GroupBy(Posts.AuthorID).
	Having(orm.Count[Post]().Gt(5)).
	All(ctx)
```

The result types are PostgreSQL's, not Go's. `sum(integer)` is `bigint`, so
`SumInt32` produces `*int64`; `sum(bigint)` is `numeric`, so `SumInt64` asks for
your configured numeric type rather than silently returning `int64` and
overflowing. Every aggregate but `count` returns a pointer, because an aggregate
over no rows is NULL — `count` is the one that is legitimately zero.

`FILTER` restricts one aggregate without touching the statement's `WHERE`, and
`.Distinct()` on an aggregate is `count(DISTINCT x)`, which is a different thing
from `SelectQuery.Distinct()` over the whole row.

`orm.Count[Post]()` is an expression. `db.Posts.Query().Count(ctx)` is the
question "how many rows would this query return" and is unchanged.

### Write expressions and RETURNING

```go
// value = value + 1, without reading it first
db.Counters.Update().
	Set(Counters.Value.SetExpr(Counters.Value.Add(1))).
	Where(Counters.ID.Eq(id)).
	Exec(ctx)

// the rows a delete removed, which nothing can select afterwards
gone, err := orm.DeleteReturning(
	db.Sessions.Delete().Where(Sessions.ExpiredAt.Lt(now)),
	SessionSummaries,
).All(ctx)

// an upsert that computes from both the old row and the rejected one
db.Users.Insert(ctx, user, orm.OnConflict(Users.Email).DoUpdateSet(
	Users.Nickname.SetExpr(orm.ExcludedNull(Users.Nickname)),
	Users.LoginCount.SetExpr(Users.LoginCount.Add(1)),
))
```

`EXCLUDED` is a typed source, not a string, and using it outside a conflict
clause is refused before the statement runs.

`UpdateReturning(...).One(ctx)` runs the **whole** write and then reports
`ErrMultipleRows` if more than one row came back. It does not add a `LIMIT`:
that would change which rows the statement modified, and a terminal's
convenience is not worth altering a mutation. Use it when the `WHERE` already
identifies one row; otherwise use `All`, which buffers the returned rows. There
is no streaming form, because a stream implies that stopping early stops the
work — and by the time the first row arrives, the write has happened.

Nullability travels with the expression, not just with the column:
`Users.Visits.Add(1)` is `*int64`, because `NULL + 1` is `NULL`. `RawValue` is
the one place you state a type yourself, and it is an explicit trust boundary —
a wrong claim fails as a scan error rather than as a wrong value.

## SQL composition

A query that reads one table is the easy half. The other half is a query that
reads several — and the idea that makes it one feature rather than five is that
**a typed query is a typed source**:

```go
userID := orm.Named("user_id", orm.Of(Posts.AuthorID))
posts  := orm.Named("post_count", orm.Of(orm.Count[orm.Composed]()))

stats := orm.Sub("post_stats", orm.Rows(userID, posts).
    From(Posts.Source()).
    GroupBy(orm.Of(Posts.AuthorID)))

rows, err := orm.Compose(db.Executor(), orm.Project3(
    orm.Of(Users.ID),
    orm.Of(Users.Email),
    orm.OptRef(stats, posts),          // *int64 — see below
    func(id int64, email string, n *int64) Row { ... },
)).
    From(Users.Source()).
    LeftJoin(stats, orm.Eq(orm.Ref(stats, userID), Users.ID)).
    All(ctx)
```

That one abstraction is what derived tables, subqueries, joins, CTEs, recursive
CTEs and `LATERAL` are all built from. They nest through one compiler, so a
statement with a CTE, a derived table, two outer joins, a correlated `EXISTS`
and a window function has **one** parameter list, numbered in the order the SQL
reads. Nothing is rendered to a string and pasted in.

The rule worth reading twice is nullability. `post_count` is `count(*)` and can
never be NULL — but read through a `LEFT JOIN` that matched nothing, it is:

> Nullability is not only a property of the column. It is a property of how the
> column's source enters *this* query.

So an outer-joined value is read with `orm.Opt` or `orm.OptRef`, which widen the
result type, and a select list that does not is **refused** rather than left to
fail as a scan error on the first parent with no child. Nothing about
`Profiles.ID` changes: it still means a `NOT NULL` column in every other query.

Also here: `CASE`, `COALESCE`, `NULLIF`, `CAST`, `DISTINCT ON`, tuple
comparisons, array and JSON operators, and window functions with `PARTITION BY`,
their own `ORDER BY` and frames. Window functions are refused in `WHERE` and
`HAVING`, because PostgreSQL computes them after both — filter on one by
computing it in a derived table and filtering outside, which is exactly what the
composition is for.

Two things that look alike and are not: `Query.With` loads relations,
`ComposedQuery.Join` composes SQL. Asking for a relation never changes which
root rows come back; writing a join does, because that is what a join is.

The whole of it — including what the Go compiler refuses, what the builder
refuses, and what is left to PostgreSQL — is in
[docs/composition.md](composition.md).

## Relations

A relation is loaded when you ask for it and not otherwise:

```go
users, err := db.Users.Query().
    Where(Users.Active.Eq(true)).
    OrderBy(Users.CreatedAt.Desc()).
    Limit(20).
    With(Users.Profile, Users.Posts).
    All(ctx)
```

`With` takes relation descriptors from the same table type the columns come
from, and they are typed the same way: `Users.Posts` is an
`orm.Rel[User, Post]`, so `db.Posts.Query().With(Users.Posts)` does not compile.

A relation you did not ask for stays unloaded, and unloaded is a state you can
observe rather than a zero value you have to guess at:

```go
if posts, ok := u.Posts.Get(); ok {
    // loaded — possibly to zero rows
} else {
    // never asked for
}
```

Nothing lazy-loads. There is no hidden query behind a field access, so a loop
over a slice of entities cannot turn into a query per iteration.

### What it costs

Loading `M` relations over any number of rows takes **1 + M statements at most**,
and the count does not depend on how many rows come back:

```
SELECT ... FROM users LEFT JOIN profiles ... WHERE ... LIMIT 20  -- root, with the to-one folded in
SELECT ... FROM posts JOIN unnest($1::int8[]) WITH ORDINALITY    -- the to-many, once for all of them
```

A to-one relation is folded into the root statement as a `LEFT JOIN`, so it
costs no statement at all. A to-many relation cannot be: joining it would
multiply the root rows and make `LIMIT 20` mean twenty *joined* rows rather than
twenty users. It gets one statement of its own, covering every parent at once.

The batched statement does not group in Go. The parent keys go in as arrays,
`WITH ORDINALITY` numbers them, and each returned child carries the ordinal of
the parent it matched:

```sql
SELECT "_k"."ord", "_c"."id", "_c"."title"
FROM unnest($1::int8[]) WITH ORDINALITY AS "_k"("k0", "ord")
JOIN "public"."posts" AS "_c" ON "_c"."author_id" = "_k"."k0"
```

That matters because Go equality is not PostgreSQL equality. `citext` compares
case-insensitively, `numeric` ignores trailing zeroes, a domain carries its base
type's semantics, and a configured type compares however its author decided.
Grouping children by key in Go would quietly mis-attach rows for every one of
them. Here PostgreSQL decides what matches what, and Go only does arithmetic on
an ordinal.

The keys travel as one array parameter per key column rather than one per
parent, so a thousand parents cost the same single parameter as two. There is no
bind-parameter ceiling to chunk against.

### Relation options

A relation can be narrowed, ordered and capped:

```go
users, err := db.Users.Query().
    With(
        Users.Posts.
            Where(Posts.Published.Eq(true)).
            OrderBy(Posts.CreatedAt.Desc()).
            Limit(5),
    ).
    All(ctx)
```

**`Where` inside `With` filters relation rows, not roots.** That is the sentence
to remember. Every user the query would have returned is still returned; a user
with no published post gets `Posts` loaded and empty. If you meant *only the
users who have published something*, that is a different query — see
[Filtering by a relation](#filtering-by-a-relation) below.

The options are typed to the **target** entity, so `Users.Posts.Where(...)`
takes a `Predicate[Post]`. Handing it a `Predicate[User]` does not compile.

`Rel` is a value and every option returns a modified copy, so the generated
descriptor is never changed and two configured copies cannot see each other:

```go
recent := Users.Posts.OrderBy(Posts.CreatedAt.Desc()).Limit(5)
popular := Users.Posts.OrderBy(Posts.Score.Desc()).Limit(5)
// Users.Posts itself still has no options at all.
```

`Where` accumulates with AND, across arguments and across calls, exactly as the
root `Query.Where` does. `OrderBy` appends, so a second call breaks the first's
ties rather than replacing it. `Limit` may be stated once: a second call is an
error rather than a silent replacement, because quietly changing a cardinality
bound somebody already wrote is how a wrong number survives review.

**`Limit` is per parent.** `Limit(5)` gives *each* user up to five posts, not
five posts shared between them, and still costs one statement. Under the hood
that is `CROSS JOIN LATERAL`, so the limit is evaluated once per parent:

```sql
SELECT "_k"."ord", "_l"."id", "_l"."title", "_l"."created_at"
FROM unnest($1::int8[]) WITH ORDINALITY AS "_k"("k0", "ord")
CROSS JOIN LATERAL (
    SELECT "posts"."id", "posts"."title", "posts"."created_at"
    FROM "public"."posts"
    WHERE "posts"."author_id" = "_k"."k0" AND "posts"."published" = $2
    ORDER BY "posts"."created_at" DESC
    LIMIT 5
) AS "_l"
ORDER BY "_k"."ord" ASC, "_l"."created_at" DESC
```

Without a limit the simpler flat join is used, filter and ordering included.
LATERAL is what per-parent limiting needs, not a tax on every relation.

Two notes on `Limit`:

- **Pair it with `OrderBy`.** Without an ordering, *which* five rows a parent
  gets is unspecified, and no ordering is invented to hide that — an ORM that
  quietly sorted by primary key would be answering a question you did not ask.
  Write `OrderBy(...).Limit(...)` whenever it matters which rows arrive.
- **`Limit(0)` is a real answer.** The relation is loaded, empty, for every
  parent, and no statement runs at all.

A configured to-one relation loads separately rather than folding, because the
predicates you wrote are against the target's own table and the folded join
reads it under a different name. Semantically nothing changes: the root row is
still there, and its relation is loaded-absent when the target does not match.

### Filtering by a relation

`Any` and `None` filter **roots**:

```go
// Users who have published something.
users, err := db.Users.Query().
    Where(Users.Posts.Any(Posts.Published.Eq(true))).
    All(ctx)

// Users who have not.
users, err := db.Users.Query().
    Where(Users.Posts.None(Posts.Published.Eq(true))).
    All(ctx)

// Users with no posts at all, and users with no profile row.
db.Users.Query().Where(Users.Posts.None())
db.Users.Query().Where(Users.Profile.None())
```

They compile to correlated `EXISTS` and `NOT EXISTS`:

```sql
SELECT ... FROM "public"."users"
WHERE EXISTS (
    SELECT 1 FROM "public"."posts"
    WHERE "posts"."author_id" = "users"."id" AND "posts"."published" = $1
)
```

which costs no extra statement, returns each root row at most once — no join, so
no deduplication — and composes with `Limit`, `Offset`, `OrderBy`, `Count`,
`Exists`, `One` and `ForUpdate`. `NOT EXISTS` is used rather than an outer join
tested for `IS NULL`: it says the same thing without changing the cardinality on
the way.

`Any` with no predicates asks only whether a related row exists. Both work on
to-one and to-many relations alike, and a belongs-to whose foreign key is NULL
has no related row, so PostgreSQL answers `Any` false and `None` true without
anything in Go deciding it.

**`Any` does not load anything.** Filtering by a relation is not asking for it:

```go
users, _ := db.Users.Query().Where(Users.Posts.Any()).All(ctx)
users[0].Posts // still unloaded
```

Wanting both is two requests, and they stay independent:

```go
db.Users.Query().
    Where(Users.Posts.Any(Posts.Published.Eq(true))).   // which users
    With(Users.Posts.Where(Posts.Published.Eq(true))).  // which posts
    All(ctx)
```

The `EXISTS` subquery is not quietly reused as the relation's own statement.
That would be a cleverness whose failure mode is a query that means something
other than what it says.

### Nested relations

A relation can load relations of its own, as deep as you write it:

```go
users, err := db.Users.Query().
    With(
        Users.Posts.
            Where(Posts.Published.Eq(true)).
            OrderBy(Posts.CreatedAt.Desc()).
            Limit(5).
            With(
                Posts.Comments.
                    OrderBy(Comments.CreatedAt.Desc()).
                    Limit(10).
                    With(Comments.Author),
            ),
    ).
    All(ctx)
```

`Users.Posts.With(...)` takes relations of **Post**, so `Users.Posts.With(Users.Profile)`
does not compile. Nesting is explicit at every level: nothing loads a relation
that was not asked for, and nothing recurses on its own.

**Every option belongs to its own level.** The `Where` above filters posts, not
users and not comments. The `Limit(5)` is five posts *per user*; the `Limit(10)`
is ten comments *per post*. An ordering sorts inside one parent's relation and
says nothing about any other level, or about the roots.

**Descendants never change their ancestors.** Which five posts a user gets is
decided before their comments are loaded, so adding `Comments.Author` cannot
change which comments arrived, and cannot change which users did. Root
pagination is the root query's own, whatever the tree below it asks for.

### What a tree costs

Loading is **breadth-first and batched**. Every post across every user is loaded
by one statement, then every comment across every post by one more:

```
Users
└── Posts
    └── Comments
        └── Author

1 root + 3 relation statements
```

The count follows the shape of the requested tree, never the number of rows in
it. One user or a hundred, five posts each or five hundred, that query is four
statements. A relation that folds into the root costs nothing at all, so
`With(Users.Profile)` alongside the tree above is still four.

Two things cost less rather than more. A relation that loaded nothing has no
rows for its own relations to attach to, so the level below it does not run —
and `Limit(0)` skips its own statement and every statement under it.

## Relations without a Go field for the key

A relation can be loaded even when the entity does not carry its foreign key:

```go
type Comment struct {
    ID   int64
    Body string

    Author orm.One[User]   // comments.author_id is nowhere in this struct
}
```

The statement that loads the comments selects `author_id` as an extra value,
holds it for exactly as long as the author loader needs it, and discards it. The
column never becomes a field, nothing is stored on the entity, and
`Users.Posts.With(Posts.Comments.With(Comments.Author))` works the same as if it
had been mapped. The ORM does not get to dictate the shape of your struct in
order to load a relation you asked for.

### What relations do not change

`With` is a request for data, not a change to the query. Everything else means
what it meant without it:

- **Pagination is the root's.** `Limit` and `Offset` count root rows, whatever a
  relation's own limit says. A user with forty posts is one of your twenty users.
- **`Count` and `Exists` ignore `With` entirely**, at every depth. Neither
  returns entities, so loading relations for them would be work whose result is
  discarded. `Any` and `None` are a different matter: they are root predicates,
  so they do count.
- **`ForUpdate` locks root rows only.** The statement compiles to
  `FOR UPDATE OF "users"`, so a relation you read is not locked as a side effect
  of reading it. Lock a table by querying it.
- **`Where` and `OrderBy` still address the root.** A relation's own `Where` and
  `OrderBy` live on the relation descriptor, and neither reaches the other.

`Rows` is the one terminal that cannot serve every relation. A folded to-one
streams, because it arrives with the row. Anything loaded after the root rows —
a to-many, a to-one that options moved out of the join, or *any* relation nested
under either — needs every root row before its statement can run, which is
exactly the buffering `Rows` exists to avoid. The whole tree is checked, so
`With(Users.Profile.With(Profiles.Avatar))` is refused even though `Profile`
itself would have streamed. The refusal is `orm.ErrStreamingRelation`, returned
before PostgreSQL is touched rather than after half a result is in memory.

`SQL()` stays the root statement. `With` may run further statements, and
returning several from a function that returns one string would be a worse API
than saying so.

### Absent, empty and unloaded

Three different things, kept apart:

```go
p, ok := u.Profile.Get()   // ok == false          — never asked for
p, ok := u.Profile.Get()   // ok == true, p == nil — loaded, and there is no such row
posts, _ := u.Posts.Get()  // len(posts) == 0      — loaded, and there are no rows
```

A to-one relation that matched nothing comes back from a `LEFT JOIN` as NULL in
every target column, which is why the generator writes a null mirror per target
rather than scanning straight into the entity. Presence is decided by the
primary key, which PostgreSQL guarantees is not NULL for a row that exists.

### What is not here yet

Relation projections and aggregates, a public arbitrary `Join`, a general
subquery API, an identity map, and any relation write. `With` materialises the
relations reconciliation proved, arranged however you write them, and nothing
else — there is no lazy loading and no relation loads itself.

Nothing is deduplicated across a tree, either. A user who is the root, the
author of a post and the author of a comment is three independent values, not
one shared pointer. Loading the same row twice is cheaper than the bookkeeping
that would avoid it, and an identity map would make the values you get back
depend on what else the query happened to load.

## Connecting

Wire type registration into the pool so every connection is taught this
package's PostgreSQL types once:

```go
cfg, err := pgxpool.ParseConfig(dsn)
if err != nil {
    return err
}
cfg.AfterConnect = domain.RegisterTypes

pool, err := pgxpool.NewWithConfig(ctx, cfg)
if err != nil {
    return err
}
db := domain.New(pool)
```

`RegisterTypes` is generated. It loads the enums, domains and composites your
entities use — types whose OIDs are assigned when they are created, so a driver
can only learn them by asking — and does nothing else. It runs no reconciliation
and reads no catalog beyond what pgx needs. A package that uses none of those
types gets a no-op, so the setup above does not change when one appears later.

`New` accepts anything with pgx's `Query` method: a `*pgxpool.Pool`, a
`*pgx.Conn` or a `pgx.Tx`. Because every statement goes through the executor you
gave it, whatever observability you have configured on pgx — a `QueryTracer`, the
OpenTelemetry integration, a logging tracer — sees all of it. This package opens
no connections of its own.

## Transactions

```go
err := db.Tx(ctx, func(tx *domain.DB) error {
    user, err := tx.Users.Insert(ctx, User{Email: email})
    if err != nil {
        return err
    }
    _, err = tx.Profiles.Insert(ctx, Profile{UserID: user.ID})
    return err
})
```

The `DB` handed to the callback is a new one bound to the transaction. Every
repository reached through it is in the transaction, and the `db` you called it
on is not — so a transaction is something you opt into rather than something
that happens to you.

Returning nil commits. Returning an error rolls back and returns your error
unchanged, so `errors.Is` still finds your own sentinel through it. A panic rolls
back and re-panics with the original value: a panic is a bug, and turning it into
an error would hide where it came from.

**Nothing is retried.** A serialization failure comes back like any other error.
Whether retrying is safe depends on what your callback did, which this package
does not know.

Nesting works and is a savepoint:

```go
db.Tx(ctx, func(tx *domain.DB) error {
    if err := tx.Tx(ctx, func(nested *domain.DB) error {
        return maybeFails()
    }); err != nil {
        // The savepoint rolled back. The outer transaction is still alive.
    }
    return nil
})
```

`TxOptions` takes pgx's own `TxOptions` — this package writes no `SET
TRANSACTION` SQL of its own:

```go
db.TxOptions(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable}, fn)
```

Non-zero options *inside* a transaction return `orm.ErrNestedTxOptions`.
PostgreSQL sets isolation and access mode for a transaction, not for a savepoint
within one, and reporting success for a change that did not happen is worse than
refusing. Zero options nest fine.

The rollback that follows a failed callback runs on a context derived with
`context.WithoutCancel` and a short timeout, because the callback often failed
*because* its context was cancelled — and a rollback on a cancelled context never
reaches the server.

## Raw

`orm.Expr` is a fragment inside a query this package builds. `orm.Raw` is the
whole statement, with the generated scanner kept:

```go
users, err := orm.Raw(db.Users, `
    SELECT id, email, name, active, created_at
    FROM users
    WHERE email ILIKE '%' || $1 || '%'
    ORDER BY length(name), name
`, term).All(ctx)
```

It has `All`, `One` and `Rows`, and returns the same `ErrNotFound` and
`ErrMultipleRows` as the typed query.

The contract is positional: the statement must return **every mapped column of
the entity, in the order the entity declares them**. `orm explain User --sql`
prints that order. Column names are not matched, so an alias is fine. The count
is checked against `FieldDescriptions` before any row is read, and a mismatch is
`orm.ErrRawColumns` rather than a scan error naming a column you never wrote.

Placeholders are PostgreSQL's own and are passed through untouched — Raw owns the
whole statement, so there is nothing to renumber into. **Pass values as
arguments.** `orm.Expr` and `orm.Raw` are the two places this package accepts SQL
text you wrote; they do not accept values you formatted into it:

```go
orm.Raw(db.Users, `SELECT ... WHERE email = $1`, email)   // yes
fmt.Sprintf(`SELECT ... WHERE email = '%s'`, email)       // no
```

A Raw query built from a repository inside a transaction runs inside that
transaction, because the repository is what decides the executor.

There is no `Raw.Count`, `Raw.Update` or `Raw.Delete`: somebody writing their own
SQL can write those, and a wrapper for them would have to rewrite a statement
this type promises not to read.

## Inspecting a mapping

```console
$ orm explain User
Entity: User
Go:     example.com/app/internal/domain.User
Table:  public.users

Columns

  Go field   Go type     PostgreSQL              Properties
  ID         int64       id int8                 PK NOT NULL identity (by default)
  Email      string      email text              NOT NULL
  Active     bool        active bool             NOT NULL default
  CreatedAt  time.Time   created_at timestamptz  NOT NULL default

Relations

  Posts orm.Many[Post]
    direction:  has-many (the target's table carries the foreign key)
    constraint: posts_author_id_fkey
    key:        public.users.id -> public.posts.author_id
    target:     example.com/app/internal/domain.Post
```

`--json` gives the same thing as structured data; `--sql` prints the statement a
plain query compiles to, which is also the column order `orm.Raw` has to return.
Output is deterministic — no timestamps, nothing read from a map — so it can be
diffed and committed.

If two configured packages both declare `User`, the bare name is refused and the
qualified candidates are listed. Naming one of them explicitly works:
`orm explain example.com/app/internal/domain.User`.

## Keeping generated code honest

`orm generate` writes an `orm.lock` beside your configuration:

```json
{
  "version": 1,
  "mapping_sha256": "45fd92e36ab9f6ea86a2407338b4910128cd63bc69cdf3d52513d2c606793ba2"
}
```

The fingerprint is over the **mapping** — the columns you mapped, their types and
nullability, what the database supplies, the primary key, the constraints your
relations were proved from, enum labels — canonically encoded and hashed. Nothing
machine-specific goes into it: no path, no host, no OID, no timestamp. The same
commit against the same schema fingerprints identically on any machine.

`orm check` compares the mapping it proves now against it:

```console
$ orm check
7 findings: 0 errors, 7 warnings

generated code is stale: it was produced from a different mapping

    run: orm generate
```

This catches the change nothing else does. A colleague adds a default to a mapped
column: your committed `.gen.go` files still compile, still run, and are now
describing a database that no longer exists. The Go compiler cannot see it.

Adding a column *nobody mapped* does not make generated code stale — it changes
nothing about what would be generated. It is still reported as an unmapped
column, as it always was.

`--generated` makes staleness, and a missing lock, fail the command. That is the
CI flag:

```sh
orm check --generated
```

Without it, a first check on a project that has never generated says nothing
about generated code, so `orm init` → `orm check` still works before there is
anything to compare.

## Writes

### A Go zero value is a value

This is the rule the write layer is built on, and it is worth stating before
anything else:

```go
db.Users.Insert(ctx, User{Email: "alex@example.com", Active: false})
```

writes `active = FALSE`. It does **not** mean "Active is unset, so let the
database decide". There is no way to tell that apart from an author who meant
`false`, and guessing wrong writes the opposite of what was asked.

A database default is a different request, and you make it explicitly:

```go
user, err := db.Users.Insert(
    ctx,
    User{Email: "alex@example.com"},
    orm.Default(Users.Active),
)
```

`NULL` is a third thing again. A nil pointer field writes `NULL`; only `Default`
omits the column.

### Insert

```go
created, err := db.Users.Insert(ctx, User{
    Email:  "alex@example.com",
    Active: false,
})
```

```sql
INSERT INTO "public"."users" ("email", "age", "active", "nickname")
VALUES ($1, $2, $3, $4)
RETURNING "id", "email", "age", "active", "nickname", "created_at", "slug"
```

Identity and generated columns are never written and always returned, so the
key PostgreSQL assigned and the columns it computed are on the result. The
entity you passed in is **not** modified — those values belong to `created`
alone.

`RETURNING` names its columns; there is no `RETURNING *`, because that would let
the server decide the scan order the generated scanner depends on.

### InsertMany

```go
created, err := db.Users.InsertMany(ctx, []User{
    {Email: "a@example.com"},
    {Email: "b@example.com"},
})
```

Rows come back in the order given. An empty slice returns an empty slice and
runs nothing.

Large inserts are split across statements, because one statement can only carry
so many bind parameters. That makes the operation several statements, and M4 has
no transaction of its own to wrap them in: pass a `pgx.Tx` as the executor when
the whole insert has to succeed or fail together. Opening one implicitly would
be a write you never asked for.

### Update

```go
affected, err := db.Users.
    Update().
    Set(
        Users.Nickname.Set("Alexander"),
        Users.DeletedAt.SetNull(),
    ).
    Where(Users.ID.Eq(id)).
    Exec(ctx)
```

```sql
UPDATE "public"."users" SET "nickname" = $1, "deleted_at" = NULL WHERE "users"."id" = $2
```

`SetNull` binds no parameter — it writes the value `NULL`, which is not the same
as `Default`, which omits a column so the database supplies one.

`Exec` returns the count PostgreSQL already reported. Nothing issues a second
statement to find it out.

Assigning the same column twice is an error rather than last-one-wins, and so is
assigning a generated or identity column — PostgreSQL would reject those, and it
should not be the first place you hear about it.

### Delete

```go
affected, err := db.Users.Delete().Where(Users.ID.Eq(id)).Exec(ctx)
```

```sql
DELETE FROM "public"."users" WHERE "users"."id" = $1
```

### Why `.All()` exists

An update or delete with no `WHERE` is **refused**:

```go
db.Users.Update().Set(Users.Active.Set(false)).Exec(ctx)  // ErrMissingWhere
db.Users.Delete().Exec(ctx)                               // ErrMissingWhere
```

The absence of a `WHERE` clause is also exactly what a forgotten one looks like,
and a full-table write is among the few mistakes nobody can undo. So neither
reading is assumed — you say which you meant:

```go
affected, err := db.Users.
    Update().
    Set(Users.Active.Set(false)).
    All().
    Exec(ctx)
```

It is verbose on purpose. Calling both `All` and `Where` is an error too:
each contradicts the other, and silently letting one win would defeat the guard.

### Upsert

```go
created, err := db.Users.Insert(
    ctx,
    input,
    orm.OnConflict(Users.Email).DoUpdate(Users.Nickname, Users.Active),
)
```

```sql
INSERT INTO ... ON CONFLICT ("email")
DO UPDATE SET "nickname" = EXCLUDED."nickname", "active" = EXCLUDED."active"
```

Only the named columns change. Updating every column would be the convenient
default and the wrong one: an upsert usually means "these fields are new, the
rest belong to the existing row".

```go
created, err := db.Users.Insert(ctx, input, orm.OnConflict(Users.Email).DoNothing())
```

PostgreSQL returns no row for an insert it discarded, so `Insert` reports
`orm.ErrConflictIgnored` rather than handing back a zero entity that looks like
a row. It does not go and fetch the row that was already there — that would be a
query you did not ask for. `InsertMany` under `DoNothing` returns the rows that
landed, so the result may be shorter than the input.

### What the write layer will not do

There is no `Save`. Nothing decides between an insert and an update by looking
at a primary key, nothing tracks which fields you changed, and nothing writes a
row you did not ask it to. Write intent is always stated.

## Errors

Every error a caller might branch on is a sentinel or a typed error, wrapped
rather than replaced. Nothing requires matching on a message:

| Error | Means |
|---|---|
| `ErrNotFound` | `One` matched nothing |
| `ErrMultipleRows` | `One` matched more than one row |
| `ErrMissingWhere` | an update or delete named no rows; call `All()` to mean every row |
| `ErrMissingSet` | an update assigns nothing |
| `ErrDuplicateAssignment` | one column assigned twice |
| `ErrInvalidDefault` | `Default` for a column PostgreSQL cannot supply |
| `ErrConflictIgnored` | an insert PostgreSQL discarded under `DoNothing` |
| `ErrStreamingRelation` | `Rows` cannot serve a relation loaded after the root rows |
| `ErrRawPlaceholder` | an `Expr` fragment whose placeholders and arguments disagree |
| `ErrRawColumns` | a `Raw` statement returning the wrong number of columns |
| `ErrNoTransaction` | an executor that cannot begin one |
| `ErrNestedTxOptions` | transaction characteristics requested inside a transaction |

`ScopeError` and `AliasCollisionError` are structured and reachable with
`errors.As`.

PostgreSQL's own errors survive every layer of context this package adds:

```go
_, err := db.Users.Insert(ctx, user)

var pgErr *pgconn.PgError
if errors.As(err, &pgErr) && pgErr.Code == "23505" {
    return ErrEmailTaken
}
```

No public API panics for a bad query, a bad relation configuration, a database
error, a scan failure or a stale lock. Builder mistakes accumulate and surface
together from the terminal operation, which is also where the query would have
run — so a query that cannot be built never reaches PostgreSQL.

## What things cost

Structural guarantees, each of which has a test that would fail if it stopped
being true:

| Query | Statements |
|---|---|
| a plain query | 1 |
| `With` a plain to-one | 1 — folded into the root as a `LEFT JOIN` |
| `With` a to-many | 1 + 1 |
| `Users → Posts → Comments → Author` | 1 + 3 |
| `Any` / `None` | 1 — correlated `EXISTS` in the root statement |
| `Count` / `Exists` with any `With` tree | 1 — `With` is ignored |
| a relation with `Limit(0)`, or one that loaded nothing | costs nothing, and neither does anything under it |

**The count follows the shape of the requested relation tree, never the number of
rows in it.** One root or a hundred, five children each or five hundred: the same
statements. There is no N+1 for any supported loading plan, and the tests assert
exact statement counts rather than upper bounds.

Relation keys travel as one array parameter per key column, so a thousand parents
cost the same one parameter as two. A per-parent `Limit` uses `LATERAL`, so it
caps each parent's rows rather than the statement's. Root pagination is never
affected by anything a relation asks for.

Reading a row uses no reflection: the scanner is a generated switch over a
constant index. Attaching related rows is indexing by ordinal — PostgreSQL
decides which rows relate, under whatever equality the key's own type defines, so
`citext`, `numeric`, domains and custom types all behave as they do in the
database rather than as Go's `==` would.

## Configuration

```yaml
version: 1

schema:
  # database: PostgreSQL owns the schema (the default).
  # managed:  the Go declarations own it and orm migrations carry it there.
  mode: database
  dsn: ${DATABASE_URL}
  search_path:
    - public

# Only read in managed mode.
migrations:
  dir: migrations

packages:
  - path: ./internal/domain
    output: same

types:
  numeric:
    go: github.com/shopspring/decimal.Decimal
    codec: decimal

strict:
  unmapped_columns: warn
  timestamp_without_tz: warn
```

The schema may come from a DDL file instead, in which case the tool creates a
throwaway database, lets PostgreSQL apply the file, introspects the result and
drops it again. There is no DDL parser here:

```yaml
schema:
  file: db/schema.sql
  admin_dsn: ${ADMIN_DATABASE_URL}
```

Because PostgreSQL executes the file, it must be plain SQL. The backslash lines
`pg_dump` emits (`\restrict`, `\connect`) are psql meta-commands, not SQL, and
the server rejects them — point `schema.file` at the DDL you maintain or at a
concatenation of your migrations.

`${NAME}` reads an environment variable. Referencing one that is not set is a
configuration error, not an empty string.

`numeric` and `uuid` have no default Go mapping on purpose. Silently mapping
`numeric` to `float64` corrupts money, and Go has no `uuid` type that everyone
agrees on, so the tool asks rather than guesses.

## Finding codes

Codes are public API. They are never renumbered and never reused.

| Code | Severity | Meaning |
|------|----------|---------|
| E001 | error | Go field has no matching column |
| E002 | error | unmapped NOT NULL column with no default or generated value |
| W003 | warning | PostgreSQL column exists but is unmapped |
| E004 | error | nullable PostgreSQL column cannot be represented by the Go field |
| W005 | warning | pointer Go field mapped to a NOT NULL column |
| E006 | error | incompatible Go and PostgreSQL types |
| E007 | error | enum labels differ |
| E008 | error | relation has no candidate foreign key |
| E009 | error | relation foreign key is ambiguous |
| E010 | error | remote-FK to-one relation is not guaranteed unique |
| E011 | error | table has no primary key |
| E012 | error | unsupported PostgreSQL type |
| E013 | error | type has no configured Go mapping |
| E014 | error | generated identifier collision |
| W015 | warning | timestamp without time zone |
| E016 | error | table does not exist |
| E017 | error | multiple Go entities map to one PostgreSQL table |
| E018 | error | multiple Go fields map to one PostgreSQL column |
| E019 | error | many-cardinality relation has a local foreign key |
| E020 | error | relation target is not a mapped entity |
| E021 | error | invalid or unknown ORM tag |
| E022 | error | view entity unsupported |
| E023 | error | primary key column has no mapped Go field |
| E024 | error | relation target is in another package |

## Entities in more than one package

`packages:` takes a list, and entities may live in as many packages as you like:

```yaml
packages:
  - path: ./catalog
    output: same
  - path: ./sales
    output: same
```

Each package gets its own generated code and its own `DB`, built from the same
schema and the same migration history. Reconciliation spans all of them — two
packages that map the same table are E017, and two whose generated identifiers
would collide are E014 — so the packages are checked together even though they
are generated apart.

**A relation may not cross a package boundary.** That is E024. The loader for
`Order.Product` is generated into `Order`'s package and needs `Product`'s
descriptors, which are generated into `Product`'s package and are unexported.

The one-way case is the only one worth discussing, because the other is not
representable at all: two entity packages referring to each other's types are an
import cycle, and Go rejects that before this tool is involved. No design that
names the target type can support a bidirectional relation across packages.

So if two entities relate, declare them in one package. If they must be
separated — different bounded contexts, different teams — cross the boundary
with the foreign key column instead of a relation:

```go
//orm:table orders
type Order struct {
    ID        int64 `orm:"pk,identity"`
    ProductID int64 // a real FK in the schema; just not a loadable relation
    Quantity  int32
}
```

The foreign key is still declared, still migrated and still enforced by
PostgreSQL. What you give up is `Order.Product` loading in one batched
statement; you fetch products by id yourself, which across a context boundary is
usually what you wanted anyway.

## Rules worth knowing

**A belongs-to imposes no uniqueness requirement.** Many posts share an author;
demanding a unique index on `posts.author_id` would reject the most common
relation in any schema. A has-one does impose one, because it claims at most one
row points back — and only a *total* unique index makes that true. A partial
unique index is never accepted as proof.

**Relation key columns need not be mapped.** The generator has already proved
the foreign key from the catalog; requiring you to restate it in Go would ask
you to duplicate what reconciliation verified. What the schema does force is
narrower and comes from the schema itself: a `NOT NULL` key with no default must
be mapped, because otherwise no row can be inserted (E002). A nullable one need
not be (W003).

**Composite key ordering is load-bearing.** `conkey` and `confkey` ordinality is
what pairs the two sides of a composite foreign key. Nothing here ever compares
column sets.

**A plain slice cannot be nullable.** `[]string` cannot tell SQL NULL from an
empty array, so a nullable `text[]` needs `*[]string`.

## Exit codes

| Code | Meaning |
|------|---------|
| 0 | no finding reached the failure threshold |
| 1 | findings at or above `--fail-on` (default `error`) |
| 1 | stale generated code under `--generated` |
| 1 | in managed mode: models ahead of the migrations, migrations not applied, or database drift |
| 1 | `orm makemigrations --check` when a migration is needed |
| 2 | the tool could not run: bad configuration, unreachable database, packages that do not compile, no entities found, an entity `orm explain` could not resolve |

"No entities found" is exit 2 rather than a clean report on purpose. A
`packages.path` pointing at the wrong directory produces a check over an empty
set, and reporting *that* as clean would be the most dangerous thing this tool
could say — it exists to be believed. A check that examined nothing has proved
nothing.

## Development

```console
$ go build ./...
$ go vet ./...
$ go test ./...
```

Tests that need a PostgreSQL server read `ORM_TEST_ADMIN_DSN` and skip when it
is unset, so the suite is green without one and meaningful with one:

```console
$ docker run -d --name orm-pg -e POSTGRES_PASSWORD=orm -e POSTGRES_USER=orm \
    -e POSTGRES_DB=orm -p 55432:5432 postgres:16-alpine
$ ORM_TEST_ADMIN_DSN='postgres://orm:orm@localhost:55432/orm' go test ./...
```

The fixture corpus lives in [testdata/fixtures](testdata/fixtures). Each
directory holds a schema, a set of entities and three golden reports.
Regenerate them with `ORM_UPDATE_GOLDEN=1`.

[internal/gendemo](internal/gendemo) is a worked example: hand-written entities,
a schema, and the generated files beside them, committed. `go build ./...`
therefore compiles generated code on every run, and the integration tests query
through it against a real database. A test regenerates the package and fails if
the committed files are stale.

The compile-fail suite in [compilefail_test.go](compilefail_test.go) writes each
invalid program into a temporary module and builds it, asserting that it does
not compile — the only way to test that a mistake is a compile error. It also
builds the valid forms, so the suite cannot pass by failing to compile
everything.

[examples/blog](examples/blog) is a runnable HTTP service over the ORM, tested
against a real database so it cannot rot. [examples/managed](examples/managed)
is the same runtime under managed mode: its schema comes from a committed
migration rather than a DDL file, and its test applies that migration through
the engine's Go API before running the queries.

[examples/production](examples/production) is a service with the operational
parts a deployment needs: the same application behind net/http, chi, Gin and
Fiber, three health endpoints that answer three different questions, graceful
shutdown proved by putting a request in flight and stopping the server under it,
and observability that is swept for bind values. [examples/hexagonal](examples/hexagonal)
is the same kind of application as ports and adapters, with the dependency
direction enforced by a test that reads the real import graph rather than by an
agreement.

Both are separate modules named outside the ORM's import path — `example.com/production`,
not `github.com/AlexAli29/orm/examples/production` — because Go's `internal/`
rule is lexical: a module nested under the ORM's path would have been allowed to
import its internals. Named as they are, the compiler refuses, which is what
makes them external consumers in a sense that does not depend on anyone's
discipline. [docs/production.md](production.md) is the guide they belong to.

The managed workflow is tested end to end in
[cmd/orm](cmd/orm): each scenario writes a module of its own, creates a
database, and runs the real commands in the order a person would — declare,
`makemigrations`, `sqlmigrate`, `migrate`, `check`, `generate` — including the
rename prompt, an atomic failure, a non-atomic failure, adopting an existing
database, and drift introduced with `ALTER TABLE` by hand.

Concurrency is audited rather than assumed: generated descriptors are shared by
every query in a program, so [concurrency_test.go](concurrency_test.go) runs the
operations that look like mutation — aliasing a table, configuring a relation,
cloning a query — from many goroutines under `-race`. A builder is mutable and
single-use; a descriptor is not, and that difference is release-blocking.

Benchmarks in [bench_test.go](bench_test.go) measure the work between the caller
and pgx — predicate construction, compilation, scanning, relation attachment.
They are for noticing regressions; nothing fails on a threshold. The placeholder
lexer and the identifier quoter have fuzz targets, which is where the rest of the
input space is covered:

```console
$ go test -run '^$' -bench . ./...
$ go test -fuzz FuzzScanPlaceholders -fuzztime 60s ./internal/expr/
$ go test -race ./...
```

See [docs/releasing.md](releasing.md) for the release checklist and
[CHANGELOG.md](CHANGELOG.md) for what is in each version.

### License

MIT. See [LICENSE](LICENSE).

### Dependency boundary

Runtime packages may depend only on the standard library and
`github.com/jackc/pgx/v5`. The generator and CLI may additionally use
`golang.org/x/tools` and a YAML parser. `dependency_test.go` enforces this.
