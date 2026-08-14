---
title: Query recipes
description: The everyday shapes, written out — filtering, paging, aggregation, upserts, search.
---

Every recipe here is complete enough to paste. `db` is the generated `*domain.DB`; `Users`, `Orders` and friends are the generated descriptors.

## Filtering

### Optional filters from a request

```go
func (s *Store) Search(ctx context.Context, f Filter) ([]User, error) {
    q := s.db.Users.Query()
    if f.Email != "" {
        q = q.Where(Users.Email.ILike("%" + f.Email + "%"))
    }
    if f.Active != nil {
        q = q.Where(Users.Active.Eq(*f.Active))
    }
    if !f.Since.IsZero() {
        q = q.Where(Users.CreatedAt.Gte(f.Since))
    }
    return q.OrderBy(Users.CreatedAt.Desc()).Limit(f.Limit).All(ctx)
}
```

No filters means no `WHERE` clause at all, not `WHERE TRUE`.

### Either/or

```go
db.Users.Query().Where(orm.Or(
    Users.Email.ILike("%@example.com"),
    Users.Email.ILike("%@example.org"),
))
```

### Anything but

```go
db.Users.Query().Where(orm.Not(Users.ID.In(banned...)))
```

### NULL versus empty

```go
db.Users.Query().Where(Users.Bio.IsNull())            // never set
db.Users.Query().Where(Users.Bio.Eq(""))              // set to empty
db.Users.Query().Where(orm.Or(
    Users.Bio.IsNull(), Users.Bio.Eq(""),
))                                                     // either
```

## Paging

### Offset paging

```go
db.Users.Query().OrderBy(Users.ID.Asc()).Limit(20).Offset(page * 20)
```

### Keyset paging

Correct on a moving table, and it stays fast at page 5000:

```go
q := db.Users.Query().OrderBy(Users.CreatedAt.Desc(), Users.ID.Desc()).Limit(20)
if cursor != nil {
    q = q.Where(orm.Or(
        Users.CreatedAt.Lt(cursor.At),
        orm.And(Users.CreatedAt.Eq(cursor.At), Users.ID.Lt(cursor.ID)),
    ))
}
```

### Total plus page, one round trip each

```go
total, err := db.Users.Query().Where(cond).Count(ctx)
page,  err := db.Users.Query().Where(cond).Limit(20).All(ctx)
```

## Aggregation

### Count per group

```go
type ByStatus struct {
    Status string
    N      int64
}

var byStatus = orm.Project2(
    Orders.Status, orm.Count[Order](),
    func(s string, n int64) ByStatus { return ByStatus{s, n} },
)

rows, _ := orm.Select(db.Orders, byStatus).
    GroupBy(Orders.Status).
    OrderBy(Orders.Status.Asc()).
    All(ctx)
```

### Only the busy groups

```go
orm.Select(db.Orders, byStatus).
    GroupBy(Orders.Status).
    Having(orm.Count[Order]().Gt(100))
```

### Aggregates over no rows

```go
var maxShape = orm.Project1(orm.Max(Orders.Total), func(v *int64) *int64 { return v })
// nil when there are no rows — max over nothing is NULL, and the type says so
```

## Relations

### The five most recent per parent

One statement, not one per user:

```go
db.Users.Query().
    With(Users.Orders.OrderBy(Orders.Placed.Desc()).Limit(5)).
    All(ctx)
```

### Parents that have a child

```go
db.Users.Query().Where(Users.Orders.Any(Orders.Status.Eq("paid")))
```

### Parents that have none

```go
db.Users.Query().Where(Users.Orders.None())
```

### Deep loading

```go
db.Users.Query().
    With(Users.Orders.With(Orders.Items.With(Items.Product))).
    All(ctx)
// four statements regardless of row counts
```

## Writing

### Upsert on a natural key

```go
db.Users.Insert(ctx, user,
    // Take the new row's values for these columns.
    orm.OnConflict(Users.Email).DoUpdate(Users.Name, Users.UpdatedAt),
)
```

### Increment without reading first

```go
db.Counters.Update(ctx).
    SetExpr(Counters.Hits, Counters.Hits.Add(1)).
    Where(Counters.Key.Eq(key)).
    Exec(ctx)
```

### Bulk load

```go
n, err := db.Events.CopyFrom(ctx, batch)
```

### Claim work safely

The queue pattern, with two workers never taking the same row:

```go
err := db.Tx(ctx, func(tx *domain.DB) error {
    jobs, err := tx.Jobs.Query().
        Where(Jobs.Status.Eq("pending")).
        OrderBy(Jobs.Created.Asc()).
        Limit(10).
        Lock(orm.ForUpdateStrong, orm.SkipLocked()).
        All(ctx)
    if err != nil {
        return err
    }
    for _, j := range jobs {
        if _, err := tx.Jobs.Update(ctx).
            Set(Jobs.Status, "running").
            Where(Jobs.ID.Eq(j.ID)).
            Exec(ctx); err != nil {
            return err
        }
    }
    return nil
})
```

## JSON and arrays

These are free functions producing a `Predicate[Composed]`, so they go in a
composed query. `orm.Opt` lifts a non-nullable column:

```go
meta := orm.Opt(Users.Meta)
tags := orm.Opt(Users.Tags)

orm.Compose(pool, shape).From(Users.Source()).Where(
    orm.JSONHasKey(meta, "plan"),
)

orm.Compose(pool, shape).From(Users.Source()).Where(
    orm.JSONContains(meta, orm.Val(map[string]any{"plan": "pro"})),
)

// the text at a path, cast to something comparable
tier := orm.CastNull(orm.JSONPathText(meta, "billing", "tier"), orm.Text)

orm.Compose(pool, shape).From(Users.Source()).Where(
    orm.ArrayContains(tags, orm.Val([]string{"go", "sql"})),
    orm.ArrayOverlaps(tags, orm.Val([]string{"go"})),
)
```

## Full text search

```go
q := orm.PlainToTSQuery(orm.English, input)

type Hit struct {
    ID    int64
    Title string
    Rank  float32
}

var hits = orm.Project3(
    Docs.ID, Docs.Title, orm.TSRank(Docs.Search, q),
    func(id int64, title string, rank float32) Hit { return Hit{id, title, rank} },
)

orm.Select(db.Docs, hits).
    Where(orm.Matches(Docs.Search, q)).
    OrderBy(orm.TSRank(Docs.Search, q).Desc()).
    Limit(20).
    All(ctx)
```

## Time and ranges

```go
db.Bookings.Query().Where(Bookings.During.Overlaps(
    orm.ClosedOpen(from, to),
))

db.Events.Query().Where(Events.At.Between(dayStart, dayEnd))
```

## Escape hatches

### A fragment inside a built query

```go
db.Users.Query().Where(
    orm.Expr[User]("age(created_at) > interval ?", "1 year"),
)
```

### A whole statement, with the generated scanner kept

```go
users, err := orm.Raw[User](db.Users, `
    SELECT * FROM users
    WHERE ctid = ANY (?)
`, ctids).All(ctx)
```

Both take SQL text deliberately. Neither takes values formatted into it.
