# Type mapping

> How a PostgreSQL type becomes a Go type, and what happens when Go has no equivalent.

Source: https://ormgo.vercel.app/en/docs/types/
Symbols: https://ormgo.vercel.app/api/orm.txt — the generated list of every exported name.

---
## Built-in scalars

These need no configuration. The Go type on the right is what the generator emits.

| PostgreSQL | Go |
| --- | --- |
| `bool` | `bool` |
| `int2`, `int4`, `int8` | `int16`, `int32`, `int64` |
| `float4`, `float8` | `float32`, `float64` |
| `text`, `varchar`, `bpchar`, `citext`, `name` | `string` |
| `bytea` | `[]byte` |
| `date`, `timestamp`, `timestamptz` | `time.Time` |
| `uuid` | *configured* |
| `numeric` | *configured* |
| `json`, `jsonb` | `orm.JSON` / `orm.JSONB` |
| `inet`, `cidr` | `netip.Prefix` / `netip.Addr` |
| `macaddr` | `net.HardwareAddr` |
| `interval` | `orm.Interval` |
| `tsvector`, `tsquery` | `orm.TSVector`, `orm.TSQuery` |
| `int4range`, `daterange`, … | `orm.Range[T]` |
| `int4multirange`, … | `orm.Multirange[T]` |
| `T[]` | `[]T` |

## Types Go does not have

Two of them are refused rather than guessed at, and the refusal is the feature.

### numeric

There is no lossless built-in Go type for an arbitrary-precision decimal, and mapping it to `float64` would silently corrupt money. So it must be configured:

```yaml
types:
  numeric:
    go: github.com/shopspring/decimal.Decimal
    codec: decimal
```

### uuid

Go has no `uuid` type, and the popular third-party ones are not interchangeable. The ORM refuses to choose; the project chooses, and the choice is the project's dependency:

```yaml
types:
  uuid:
    go: github.com/google/uuid.UUID
    codec: uuid
```

The ORM itself never depends on `google/uuid`. That is checked in CI, because a mandatory uuid dependency is exactly what configured mappings exist to avoid.

## The one asymmetry worth knowing

A configured mapping works in one direction.

| Mode | Tag | Result |
| --- | --- | --- |
| Database-first | none | works — `uuid` → `uuid.UUID` |
| Managed | none | **refused** — no PostgreSQL type for `uuid.UUID` |
| Managed | `pgtype:uuid` | works |

Database-first starts from a PostgreSQL type and looks the Go one up, so the mapping applies on its own. Managed starts from the Go type and there is no reverse lookup — a configuration mapping two Go types onto one PostgreSQL type would have two answers and no way to choose. So managed mode has to be told:

```go
ID   uuid.UUID   `orm:"pk,pgtype:uuid"`
Tags []uuid.UUID `orm:"pgtype:uuid[]"`
```

## Domains

Domains are supported generically: reconciliation follows a domain to the type it is built on, so a column typed `tenant_uuid` over `uuid` is served by the one configured `uuid` mapping without an entry of its own.

Name it schema-qualified. The unqualified spelling migrates and then reads back qualified, and the two do not compare equal, so an unchanged project reports drift:

```go
TenantID uuid.UUID `orm:"pgtype:public.tenant_uuid"` // right
TenantID uuid.UUID `orm:"pgtype:tenant_uuid"`        // reports permanent drift
```

## Ranges keep their bounds

A pair of endpoints cannot say whether a bound is inclusive, exclusive or unbounded, so `Range[T]` carries the whole model. Which of `daterange`, `tsrange` and `tstzrange` a `Range[time.Time]` column is comes from the catalog rather than being guessed from Go.

```go
r := orm.ClosedOpen(start, end)
db.Bookings.Query().Where(Bookings.During.Overlaps(r))
```

Values PostgreSQL canonicalises — discrete ranges, every multirange — come back as the server holds them.

## Interval is not a Duration

`Interval` keeps months, days and microseconds apart, and refuses to become a `time.Duration` when it holds a calendar component. A month has no fixed length, and the error says so rather than quietly picking 30 days.

```go
d, err := iv.Duration()
if errors.Is(err, orm.ErrCalendarInterval) {
    // months or days are present; the caller decides what they mean
}
```

## Unsupported types are refused

A column whose type has no mapping stops generation with a diagnostic naming the column, the type and the fix. It never degrades to `any`, `string` or `[]byte` — a placeholder that scans is worse than a build failure, because it fails later and further away.

## Worked examples

### Money, without float

```go
//orm:table public.invoices
type Invoice struct {
    ID    int64   `orm:"pk,identity"`
    Cents int64                                  // the simple answer
    Total decimal.Decimal `orm:"pgtype:numeric"` // the exact one
}
```

Integer cents is fine until you need a third decimal place or a rate. `numeric`
is exact at any scale, and mapping it requires a `types.numeric` entry — the ORM
will not pick a decimal package for you.

### Addresses and networks

```go
//orm:table public.sessions
type Session struct {
    ID     int64        `orm:"pk,identity"`
    Client netip.Addr   `orm:"pgtype:inet"`
    Subnet netip.Prefix `orm:"pgtype:cidr"`
    Device net.HardwareAddr `orm:"pgtype:macaddr"`
}
```

These order and index as addresses rather than as text, so a range of a subnet is
a range and not a `LIKE`.

### Arrays that mean something

```go
//orm:table public.articles
type Article struct {
    ID      int64      `orm:"pk,identity"`
    Tags    []string                        // NOT NULL, may be empty
    Authors *[]int64                        // nullable: no list at all
}
```

An empty array and a NULL array are different values and the ORM keeps them
different. Which you want is a schema decision, and the pointer is how you say it.

### A domain, so the type carries the rule

```go
// CREATE DOMAIN email AS citext CHECK (VALUE ~ '@');
type Contact struct {
    Address string `orm:"pgtype:public.email"`
}
```

Reconciliation follows the domain to `citext` and maps it to `string`. Name it
schema-qualified, or the migrated and introspected names will not compare equal.
