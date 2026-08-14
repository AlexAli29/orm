---
title: JSON and JSONB
description: Reading into a document, and why every reader comes back nullable.
---

## The column

A `jsonb` column maps to whatever Go type you declare for it — commonly a map,
sometimes a struct:

```go
//orm:table public.users
type User struct {
    ID       int64          `orm:"pk,identity"`
    Settings map[string]any `orm:"pgtype:jsonb"`
    Profile  *Profile       `orm:"pgtype:jsonb"`
}
```

`jsonb` is the one to use. `json` stores the original text including whitespace
and duplicate keys; `jsonb` stores a parsed structure, which is what indexes and
containment operators need.

## Everything is a free function

The readers and tests are free functions rather than methods, because either side
can be a column or an expression. They take an `Optional`, so a non-nullable
column is lifted with `orm.Opt`:

```go
meta := orm.Opt(Users.Settings)
```

They produce `Predicate[Composed]` or an `Expression`, so they belong in a
composed query.

## Tests

```go
orm.JSONHasKey(meta, "plan")                    // ?
orm.JSONHasAnyKeys(meta, "plan", "tier")        // ?|
orm.JSONHasAllKeys(meta, "plan", "tier")        // ?&
orm.JSONContains(meta, orm.Val(v))              // @>
orm.JSONContainedBy(meta, orm.Val(v))           // <@
orm.JSONPathExists(meta, "$.billing.tier")      // @?
orm.JSONMatches(meta, "$.age > 18")             // @@
```

`JSONPathExists` and `JSONMatches` take SQL/JSON path syntax, which is the
expressive one: `$.items[*].price`, filters, wildcards.

## Readers

```go
orm.JSONGet(meta, "billing")            // ->   a key,   returns jsonb
orm.JSONText(meta, "plan")              // ->>  a key,   returns text
orm.JSONIndex(meta, 0)                  // ->   an index of an array
orm.JSONIndexText(meta, 0)              // ->>  an index
orm.JSONPathGet(meta, "billing", "tier")  // #>  a path, returns jsonb
orm.JSONPathText(meta, "billing", "tier") // #>> a path, returns text
orm.JSONArrayLength(meta)               // jsonb_array_length
orm.JSONTypeOf(meta)                    // jsonb_typeof
```

**Every one of them is nullable, and that is not caution.** `->` over a key that
is not there is NULL. `->>` over a non-existent path is NULL. `jsonb_typeof` of
a NULL document is NULL. A document is a shape nobody validated, so a reader
that promised a non-null result would be lying about the common case.

That is why you cast rather than compare directly:

```go
age := orm.CastNull(orm.JSONPathText(meta, "profile", "age"), orm.Integer)
// Expression[*int32, *int32]
```

## Writers

```go
orm.JSONSet(Users.Settings, []string{"billing", "tier"}, v, true)
orm.JSONInsert(Users.Settings, []string{"tags", "0"}, v, false)
orm.JSONStripNulls(Users.Settings)
```

`JSONSet`'s last argument is `create_missing` — whether to add the key when the
path does not exist. `JSONInsert`'s is `insert_after`. Both are booleans in
PostgreSQL's own signature, and they are passed through rather than renamed,
because a reader checking the manual should find the same argument.

They return a `Value`, so they belong in an update:

```go
db.Users.Update(ctx).
    SetExpr(Users.Settings,
        orm.JSONSet(Users.Settings, []string{"plan"}, newPlan, true)).
    Where(Users.ID.Eq(id)).
    Exec(ctx)
```

## Indexing

A containment query wants a GIN index:

```go
//orm:index users_settings_gin_idx (Settings) using gin
```

`jsonb_path_ops` is smaller and faster for `@>` alone but supports fewer
operators — declare it as an expression index when you want it.
