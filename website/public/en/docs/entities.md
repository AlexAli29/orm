# Entities and tags

> The directives and struct tags the generator reads.

Source: https://ormgo.vercel.app/en/docs/entities/
Symbols: https://ormgo.vercel.app/api/orm.txt — the generated list of every exported name.

---
## Directives

A directive is a comment above a type. It says what the type is.

```go
//orm:table public.users
type User struct { /* ... */ }

//orm:view public.active_users
//orm:definition `SELECT id, email FROM users WHERE active`
//orm:depends-on public.users
type ActiveUser struct { /* ... */ }

//orm:materialized-view public.user_summaries
//orm:definition `SELECT user_id, count(*) AS orders FROM user_orders GROUP BY user_id`
//orm:depends-on public.user_orders
//orm:index user_summaries_key (UserID) unique
type UserSummary struct { /* ... */ }
```

The relation name is schema-qualified. `public` is not assumed, because a project with two schemas would then have two meanings for one name.

## The tag grammar

Everything else is a struct tag under the `orm` key:

| Directive | Means |
| --- | --- |
| `pk` | Part of the primary key |
| `identity` | PostgreSQL generates the value (`identity:always` for `GENERATED ALWAYS`) |
| `unique` | A single-column unique constraint |
| `column:name` | The column name, when it differs from the field |
| `pgtype:uuid` | The PostgreSQL type, when the Go type cannot imply it |
| `type:name` | A configured type-mapping key |
| `default:expr` | The column's `DEFAULT` |
| `generated:expr` | A generated column |
| `fk:user_id` | The foreign key column backing a relation |
| `side:...` | Which side of a relation this field is |
| `-` | Ignore this field entirely |

```go
//orm:table public.orders
//orm:index orders_user_idx (UserID)
type Order struct {
    ID         uuid.UUID  `orm:"pk,pgtype:uuid"`
    UserID     uuid.UUID  `orm:"pgtype:uuid"`
    Label      string     `orm:"column:title"`
    Total      string     `orm:"pgtype:numeric"`
    CreatedAt  time.Time  `orm:"default:now()"`
    Internal   string     `orm:"-"`

    User orm.One[User] `orm:"fk:user_id"`
}
```

## Nullability

A pointer is a nullable column. There is nothing else to learn:

```go
Bio        *string     // bio text
OptionalID *uuid.UUID  // optional_id uuid
Tags       []string    // tags text[] NOT NULL
```

An empty slice and a NULL array are different values, and the ORM keeps them different. If the array column is nullable, use `*[]string`.

## Zero values are values

```go
db.Users.Insert(ctx, User{Active: false})               // stores FALSE
db.Users.Insert(ctx, User{}, orm.Default(Users.Active)) // stores the column default
```

The library cannot tell "false" from "a field somebody left alone", and guessing is how a row ends up with a value nobody chose. Asking for the default is a separate, explicit thing.

## Relations

`One` and `Many` declare relations and record three states: unloaded, loaded and empty, loaded and present.

```go
type User struct {
    ID     int64 `orm:"pk,identity"`
    Orders orm.Many[Order]
}

type Order struct {
    ID     int64 `orm:"pk,identity"`
    UserID int64
    User   orm.One[User] `orm:"fk:user_id"`
}
```

The zero value is unloaded, so a struct literal that omits a relation says "I did not ask for this" rather than "there is nothing there". Reading one is `Get() ([]T, bool)` or `MustGet()`.

## Indexes

Declared on the type, because an index belongs to a relation rather than to a column:

```go
//orm:index users_email_key (Email) unique
//orm:index users_active_idx (Active, CreatedAt)
//orm:index users_lower_email_idx ("lower(email)")
//orm:index users_tags_gin_idx (Tags) using gin
//orm:index users_paid_idx (CreatedAt) where "paid_at IS NOT NULL"
```

Fields are named by their Go names; a quoted string is a SQL expression.

## Worked examples

### A multi-tenant table

Everything a tenant column needs: the tag, the composite key and the index that
makes lookups scoped rather than filtered.

```go
//orm:table public.documents
//orm:index documents_tenant_idx (TenantID, UpdatedAt)
//orm:index documents_slug_key (TenantID, Slug) unique
type Document struct {
    TenantID  int64     `orm:"pk"`
    ID        int64     `orm:"pk,identity"`
    Slug      string
    Title     string
    Body      *string
    UpdatedAt time.Time `orm:"default:now()"`
}
```

Two `pk` fields are a composite key. The unique index is on the pair, so two
tenants may use the same slug and one tenant may not.

### A table that names its columns differently

```go
//orm:table billing.invoice_lines
type InvoiceLine struct {
    ID        int64  `orm:"pk,identity"`
    InvoiceID int64  `orm:"column:inv_id"`
    Cents     int32  `orm:"column:amount_cents"`
    Note      string `orm:"-"`   // not a column at all
}
```

`column:` is for a schema you did not choose. `-` is for a field that is yours
alone — a cached value, a formatting helper — and the generator will not look
for it.

### Generated and defaulted columns

```go
//orm:table public.people
type Person struct {
    ID       int64  `orm:"pk,identity:always"`
    First    string
    Last     string
    Full     string    `orm:"generated:first || ' ' || last"`
    JoinedAt time.Time `orm:"default:now()"`
    Ref      uuid.UUID `orm:"pgtype:uuid,default:gen_random_uuid()"`
}
```

`identity:always` means PostgreSQL refuses a value you supply, which is stronger
than the default `identity`.

### Indexes worth declaring

```go
//orm:index orders_open_idx (PlacedAt) where "shipped_at IS NULL"
//orm:index orders_lower_ref_idx ("lower(reference)")
//orm:index orders_tags_gin_idx (Tags) using gin
```

A partial index over open orders is smaller than one over all of them, and stays
small as the table grows.
