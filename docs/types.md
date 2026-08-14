# PostgreSQL's own types

The rule this package follows is: do not hide PostgreSQL, make PostgreSQL
type-safe. Where PostgreSQL has a type Go does not, the type is kept rather than
reduced to something Go already has — because the reduction is where the wrong
answers come from.

This document covers ranges, multiranges and intervals. The network types,
JSONB and full text search follow the same rule and are documented on their own
functions.

## Ranges

### The model

A range is not a pair of endpoints. PostgreSQL's model has more states, and
every one of them is reachable from SQL:

```
[1,10)   a finite range, inclusive below and exclusive above
(,10)    no lower bound
[1,)     no upper bound
(,)      no bound at all
empty    a range containing nothing
```

A `struct { Start, End T }` cannot say which of those it is. `orm.Range[T]`
carries a bound kind per side and treats empty as a state of the whole value:

```go
type BoundKind uint8

const (
    BoundEmpty BoundKind = iota // the empty range, on both sides or neither
    BoundUnbounded              // no bound on this side
    BoundInclusive              // the bound value is in the range
    BoundExclusive              // the bound value is outside the range
)
```

The zero `Range[T]` is the empty range. Of the two candidates it is the safe
one: an accidental zero contains nothing rather than everything.

### Constructors

```go
orm.Closed(lo, hi)        // [lo,hi]
orm.Open(lo, hi)          // (lo,hi)
orm.ClosedOpen(lo, hi)    // [lo,hi)   the form most intervals want
orm.OpenClosed(lo, hi)    // (lo,hi]
orm.RangeFrom(lo)         // [lo,)
orm.RangeUntil(hi)        // (,hi)
orm.EmptyRange[T]()       // empty
orm.UnboundedRange[T]()   // (,)
orm.NewRange(lo, loKind, hi, hiKind)  // the general form
```

Read a value back with `IsEmpty`, `LowerBound`, `UpperBound` and `Bound`. A
bound with no value carries the zero `T`, so two equal ranges compare equal as
Go values.

### Empty is not NULL, and unbounded is neither

Three different things, and the Go types keep them apart:

| PostgreSQL | Go | `isempty()` |
| --- | --- | --- |
| `NULL` | `nil` `*Range[T]` | `NULL` |
| `empty` | `Range[T]` with `IsEmpty()` true | `true` |
| `(,)` | `Range[T]` with both bounds `BoundUnbounded` | `false` |

SQL NULL is deliberately not a state inside `Range[T]`. Nullability is already
spelled in the Go type — a `NOT NULL` column is `Range[T]` and a nullable one is
`*Range[T]` — and a `Valid` field would be a second, quieter way to say the same
thing with nothing to stop the two disagreeing.

It is also why `Range[T]` wraps pgx's range protocol rather than being an alias
for `pgtype.Range[T]`: that type's `Valid` field would make the zero `Range[T]`
mean SQL NULL, so a struct literal for a `NOT NULL` column would fail at the
database rather than in the compiler.

### Canonicalisation

PostgreSQL rewrites ranges over **discrete** types — `int4range`, `int8range`,
`daterange` — on the way in:

```
[1,10]  is stored as  [1,11)
(1,10)  is stored as  [2,10)
[1,1)   is stored as  empty
```

**Continuous** types — `numrange`, `tsrange`, `tstzrange` — keep the bounds as
written. So a round trip returns the value PostgreSQL holds, which for a
discrete range may not be the one that was sent. That is PostgreSQL's semantics
and this package does not hide it. Canonicalisation is idempotent, so the
canonical form is a fixed point: sending it back gives it back.

### The families

| PostgreSQL | Go |
| --- | --- |
| `int4range` | `orm.Range[int32]` |
| `int8range` | `orm.Range[int64]` |
| `numrange` | `orm.Range[T]` for the configured `numeric` type |
| `daterange` | `orm.Range[time.Time]` |
| `tsrange` | `orm.Range[time.Time]` |
| `tstzrange` | `orm.Range[time.Time]` |

#### daterange vs tsrange vs tstzrange

The last three share a Go element type and remain **three different PostgreSQL
types**. Which one a column has is never inferred from Go:

- **Database-first**: the catalog is authoritative. Reconciliation reads the
  family from `pg_type` and checks the Go side can hold its subtype.
- **Managed**: a `Range[time.Time]` becomes `tstzrange` unless a `pgtype:` tag
  names another. That is the same rule a bare `time.Time` field already
  follows — a wall clock with no offset denotes a different instant to everyone
  who reads it — applied one level in.

```go
//orm:table bookings
type Booking struct {
    Period orm.Range[time.Time]                           // tstzrange
    Stay   orm.Range[time.Time] `orm:"pgtype:daterange"`  // daterange
    Shift  orm.Range[time.Time] `orm:"pgtype:tsrange"`    // tsrange
}
```

It is exactly the arrangement `inet` and `cidr` already have, one level further
in.

Changing a column from `tsrange` to `tstzrange` is a change the migration engine
sees. Applying it needs a `USING` expression written out, because PostgreSQL has
no cast between the two at all: converting the bounds means choosing a time
zone, and only the project knows which.

#### numrange

`numrange` reaches its element through the configured `numeric` mapping, for the
same reason a `numeric` column does. A project with no `types.numeric` entry in
`orm.yaml` gets a diagnostic naming the element rather than a lossy `float64`:

```
error: E012: public.payments.price has the unsupported type numrange
    reason: numrange ranges over numeric, and numeric has no configured Go
            mapping: there is no lossless built-in Go type for an
            arbitrary-precision decimal, and mapping it to float64 would
            silently corrupt money.
```

### Querying

The generated descriptor is `RangeCol[E, T]` or `NullRangeCol[E, T]`, where `T`
is the **element** type — which is what makes passing a `time.Time` to an
integer range a compile error.

```go
db.Bookings.Query().
    Where(Bookings.Period.Overlaps(requested)).
    All(ctx)
```

| Method | SQL | Meaning |
| --- | --- | --- |
| `Contains(v)` | `@>` | the range holds the element |
| `ContainsRange(r)` | `@>` | the range holds the whole of the other |
| `ContainedBy(r)` | `<@` | |
| `Overlaps(r)` | `&&` | they have an element in common |
| `StrictlyLeftOf(r)` | `<<` | |
| `StrictlyRightOf(r)` | `>>` | |
| `NotRightOf(r)` | `&<` | does not extend right of the other |
| `NotLeftOf(r)` | `&>` | does not extend left of the other |
| `Adjacent(r)` | `-\|-` | they touch without overlapping |

`OverlapsCol` and `ContainsCol` compare two columns, which is what an alias
exists for. The package-level `RangeContains`, `RangeOverlaps` and the rest take
expressions on both sides, for derived tables, CTEs and anything a composed
query produced.

#### Why the descriptors carry catalog type names

A range operator is overloaded on both sides: `@>` is `(anyrange, anyelement)`
and `(anyrange, anyrange)`, and since PostgreSQL 14 there are multirange forms
too. A bind parameter arrives with no type, so PostgreSQL picks an overload
without knowing what is in it — and it picks wrong, resolving `quota @> $1` to
the range-against-range form and then failing to encode an `int32` as an
`int4range`.

So generated descriptors carry the catalog names of the types they deal in, and
every operand they build is cast to the one that is meant. The names come from
the catalog during generation. Nothing is inferred from the Go type and nothing
relies on which overload PostgreSQL would have guessed.

### The functions, and their nullability

```go
Bookings.Quota.Lower()      // Value[Booking, *int32]
Bookings.Quota.Upper()      // Value[Booking, *int32]
Bookings.Quota.IsEmpty()    // Value[Booking, bool]
Bookings.Quota.LowerInc()   // Value[Booking, bool]
Bookings.Quota.UpperInc()   // Value[Booking, bool]
Bookings.Quota.LowerInf()   // Value[Booking, bool]
Bookings.Quota.UpperInf()   // Value[Booking, bool]
```

**`lower()` and `upper()` are nullable even over a `NOT NULL` column.** A `NOT
NULL int4range` can still hold `empty` or `(,)`, for which there is no bound to
return. Typing them from the column's constraint would be wrong for two of the
six shapes a range can take, and would fail at the scan rather than at compile
time.

The other five are total over a non-NULL range — including `empty`, for which
`isempty` is true and the four bound tests are false — so they are non-nullable
over a non-nullable column, and nullable only over a `NullRangeCol`.

### Indexes

A GiST index over a range column is an ordinary index declaration:

```go
//orm:index bookings_period_gist (Period) using gist
```

It round-trips through the managed workflow with zero drift. Nothing about the
index machinery is range-aware, and the query builder never chooses an index:
that is the planner's job, and query correctness is the contract.

## Multiranges

`orm.Multirange[T]` is `[]orm.Range[T]`. The six families are
`int4multirange`, `int8multirange`, `nummultirange`, `datemultirange`,
`tsmultirange` and `tstzmultirange`, mapping by the same rules their range
counterparts do.

### PostgreSQL owns the contents

On the way in PostgreSQL sorts the components, merges any that overlap or touch,
and drops the empty ones:

```
{[1,5),[3,9)}    arrives as  {[1,9)}
{[1,5),[5,9)}    arrives as  {[1,9)}    int4 is discrete, so these are adjacent
{[10,20),[1,5)}  arrives as  {[1,5),[10,20)}
{[1,5),empty}    arrives as  {[1,5)}
```

**A round trip preserves the set of values, not the layout of the slice.**
Comparing a multirange before and after a write means comparing it to the
canonical form. This package makes no promise it does not keep.

A `nil` `Multirange[T]` is SQL NULL and an empty non-nil one is `'{}'` — the
same distinction PostgreSQL draws. A nullable multirange column is still
`*Multirange[T]`, so the two ways of writing NULL never both apply to one field.

### Operators

`Contains`, `ContainsRange`, `ContainsMultirange`, `ContainedBy`, `Overlaps`,
`OverlapsRange`, plus `IsEmpty` and `Merge` (PostgreSQL's `range_merge`, which
returns a *range* — the smallest one spanning every component).

The set operations `+`, `*` and `-` are deliberately absent. They return a
multirange rather than a boolean, which makes them projections rather than
predicates, and typing an anonymous multirange in a select list is a decision
nothing needs yet.

## Intervals

An interval is not a duration. PostgreSQL stores three components and stores
them separately on purpose:

```go
type Interval struct {
    Months       int32  // a calendar step
    Days         int32  // a civil day, 24 hours except when it is not
    Microseconds int64  // exact elapsed time
}
```

The three answer to different calendars. Adding one month to 31 January is 28
February and to 31 March is 30 April, so a month is not a fixed number of days.
Adding one day across a daylight-saving boundary is 23 or 25 hours, so a day is
not a fixed number of hours. Only microseconds are exact.

### Why not time.Duration

A `Duration` is a count of nanoseconds and has nowhere to put "one month".
Writing one as 30 days produces a value that gives the wrong answer for most of
the dates it is added to:

```
'2024-01-31'::date + '1 month'::interval   =  2024-02-29
'2024-01-31'::date + '30 days'::interval   =  2024-03-01
```

Choosing that silently is the kind of convenience that corrupts data.

### The conversions

```go
func (i Interval) Duration() (time.Duration, error)
func IntervalFromDuration(d time.Duration) Interval
```

`Duration` succeeds only when months and days are both zero, and returns
`ErrCalendarInterval` otherwise. There is no correct number to return for a
calendar component without knowing the instant it is being added to, and this
function does not know it. An interval with calendar components is not a failure
to work around — it means something a `Duration` cannot mean, and the way to use
it is to send it to PostgreSQL.

`IntervalFromDuration` is exact in the other direction: a duration has no
calendar components to lose, and everything lands in `Microseconds`.
Sub-microsecond precision is the one thing that does not survive, because
PostgreSQL intervals have none, so it truncates towards zero.

The microsecond constants `orm.Micros`, `Millis`, `Seconds`, `Minutes` and
`Hours` make a literal readable:

```go
orm.Interval{Months: 3, Microseconds: 90 * orm.Minutes}
```

There is no `Days` or `Months` constant: those are separate fields precisely
because they are not a number of microseconds.

### Arithmetic

```go
orm.AddInterval(t, iv)       // timestamp + interval
orm.SubInterval(t, iv)       // timestamp - interval
orm.IntervalPlus(a, b)       // interval + interval
orm.IntervalMinus(a, b)      // interval - interval
orm.IntervalTimes(iv, f)     // interval * numeric
```

Each has a `Null` form taking `Optional` operands and producing a nullable
result, because in SQL an operator with a NULL operand is NULL.

The result types are PostgreSQL's own, including the one that surprises people:

| Expression | Result |
| --- | --- |
| `timestamptz + interval` | `timestamptz` |
| `timestamp + interval` | `timestamp` |
| `date + interval` | `timestamp`, **not** `date` |
| `interval + interval` | `interval` |
| `interval * numeric` | `interval` |

Adding an interval to a date promotes it to a timestamp, because the interval
may carry a time component the date cannot hold. Go sees `time.Time` on both
sides, so the promotion costs nothing at the Go level and hiding it would only
make the SQL harder to predict.

There is no calendar DSL and there will not be one. Adding a month is
PostgreSQL's job, it gets 31 January right, and reimplementing it in Go would
produce a second set of rules to keep in agreement with the first.

## Outer joins

A range, multirange or interval read through an outer join can be NULL whatever
its column constraint says, and the composition layer requires it to be read
that way:

```go
orm.Opt(Bookings.Period)   // *orm.Range[time.Time]
orm.Opt(Bookings.Lease)    // *orm.Interval
```

`lower()` is already nullable, so its nullable form is itself and it is lifted
with `orm.OfNull` rather than `orm.Opt` — `Opt` would type it `**int32`, which
is a compile error at the destination rather than a wrong value.

## Known limitation: type aliases

**A Go type alias is not recognised.** Since Go 1.23 an alias is its own
`go/types` node rather than the type it names, and this scanner asks for a
`*types.Named`. So:

```go
type Period = orm.Range[time.Time]   // NOT recognised as a range
```

reaches the scanner carrying neither a qualified name nor type arguments, and
will not be mapped. Write the instantiation directly, or declare a defined type
over it:

```go
type Period orm.Range[time.Time]     // a distinct type, recognised as its own
```

This is pre-existing scanner behaviour rather than something ranges introduced,
and it is documented here rather than worked around because a general alias
model is a larger change than this milestone.

## PostGIS

`geometry` and `geography` are mapped, through the opt-in `orm/postgis` package.
They are not in the table above because they are not built in: a project that
does not import the package never sees them. See [postgis.md](postgis.md) for
the spatial types, the coordinate systems, the dimensionalities and what each
one costs to get wrong.

## Still unmapped

`bit`, `varbit`, `ltree` and composite types are not mapped. An unmapped type is
reported by `orm check` with a reason, never guessed at.

## Views and materialized views

A view is a query source. `db.ActiveUsers.Query()` composes exactly as a table's
query does — the same builder, the same compiler, the same source identity — and
the generated type says what the relation can do:

| relation | reads | writes | refresh |
|---|---|---|---|
| table | yes | `Insert`, `Update`, `Delete`, `CopyFrom` | — |
| view | yes | none | — |
| materialized view | yes | none | `Refresh` |

The writes are *absent*, not failing. A view has no `Insert` method, so the
mistake is a compile error rather than a runtime error on the path least likely
to be tested. PostgreSQL will accept writes through some views; exposing that is
a larger contract than this milestone signs.

### The short-form projection helpers are table-shaped

`orm.Select` and `orm.SelectFrom` take a `*orm.Repo[E]`, so they do not accept a
`ViewRepo` or a `MaterializedViewRepo`. Every query the ORM can express over a
view is still available through `orm.Compose` and `orm.Rows`, which take a
`*orm.Source` and therefore never needed to know what kind of relation they were
reading. This is a gap in the convenience layer, not in query semantics.

It is left as it is on purpose. Widening `Select` means introducing a common
readable-source abstraction into the public API, and that is a permanent surface
decision the v1 API review should make deliberately rather than one this
milestone should make in passing.

### Declaring a view in managed mode

```go
//orm:view public.active_users
//orm:definition `SELECT id, email FROM users WHERE active`
//orm:depends-on public.users
type ActiveUser struct {
    ID    int64
    Email string
}
```

A definition is required: a view with none is not a view, and it is refused as
`E025` when the schema is built rather than when DDL is rendered.

Definitions come in two spellings, and the choice is about length. Inline SQL
lives in the directive, in either Go quoting form — prefer backticks, since SQL
is full of quotes and a raw string escapes none of them. A `//` comment ends at
a newline, so anything multi-line goes in a file instead:

```go
//orm:view public.active_users
//orm:definition ./sql/active_users.sql
//orm:depends-on public.users
```

The path is relative to the package directory and may not leave it or be
absolute, so the same source produces the same schema on every machine.

### Dependencies are declared, never inferred

`//orm:depends-on` is authoritative for managed ordering. Nothing reads the SQL
to find dependencies: a definition can name a relation inside a CTE, behind a
function, through a quoted identifier or not at all, and a text search would be
wrong in whichever direction happened to be least safe. Database-first
introspection uses `pg_depend`, which is the catalog's own answer.

A dependency naming no declared relation is `E027`, a self-dependency is `E028`,
and a cycle is `E029`.

### Raw definitions are developer-authored schema SQL

A raw definition is source code. It is written by a developer, reviewed in a
pull request, and never receives a runtime value: PostgreSQL stores a view's
definition as a parsed query, not as a template with parameters. Any literal in
one is a constant the author put there on purpose.

This is a different question from M14's runtime bind privacy, and the answer is
different. Bind values are redacted everywhere because they carry request data.
A view definition is not redacted anywhere, because it is schema — it appears in
migrations, in `pg_get_viewdef`, and in any tool that reads either. Do not put a
secret in one.

### Materialized views: creation policy and runtime state

`//orm:with-no-data` asks for `CREATE MATERIALIZED VIEW ... WITH NO DATA`. The
default is `WITH DATA`, which is PostgreSQL's own and is what somebody who says
nothing almost always means — a view created empty is unreadable until something
refreshes it.

This is creation policy. It is not whether the view currently holds rows, which
is runtime state the server owns and which changes every time anybody refreshes.
A schema that recorded the latter would report drift after every refresh.

### Definition identity is server-canonical

Two definitions are compared through PostgreSQL's own reconstruction
(`pg_get_viewdef`), so reformatting, reindenting and comments do not read as
drift, and a changed predicate does. The ORM does not normalise SQL and does not
claim that two syntactically different formulations are equivalent.

The deparser is not stable across PostgreSQL majors — 16 stopped qualifying
columns it does not need to, so the same view reads `SELECT xt.id FROM xt` on 14
and 15 and `SELECT id FROM xt` on 16 and later. Canonical text therefore
compares one database against the project, where both sides come from the same
server and the version cancels out. It never enters a lock file or anything else
committed.
