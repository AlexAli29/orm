# PostGIS

Spatial support lives in `orm/postgis`. It is opt-in: a project that does not
import it never sees a spatial API, and the root package knows nothing about
geometry.

Everything below is asserted by a test against a real PostGIS. Where this
document says PostGIS does something, a test asked it.

```sh
go get github.com/AlexAli29/orm/postgis
```

## Contents

- [Two types, not one](#two-types-not-one)
- [Registering the codec](#registering-the-codec)
- [Declaring a spatial column](#declaring-a-spatial-column)
- [SRID](#srid)
- [Dimensionality](#dimensionality)
- [NULL and EMPTY](#null-and-empty)
- [Predicates](#predicates)
- [Distances and units](#distances-and-units)
- [Shapes that change](#shapes-that-change)
- [Reading and writing](#reading-and-writing)
- [Indexes](#indexes)
- [Migrations](#migrations)
- [What is not supported](#what-is-not-supported)

## Two types, not one

PostgreSQL has two spatial types and they answer differently:

| | `geometry` | `geography` |
|---|---|---|
| surface | a plane | the spheroid |
| `ST_Distance` returns | the coordinate system's units | metres |
| operations available | all of them | the few PostGIS defines |

They are two Go types for the same reason:

```go
Location postgis.Geometry  `orm:"pgtype:geometry(Point,4326)"`
Spot     postgis.Geography `orm:"pgtype:geography(Point,4326)"`
```

Mixing them does not compile. `Places.Location.Expr().Intersects(Places.Spot.Expr())`
is rejected by Go, not by a run-time check, and the compile-fail suite asserts
it stays that way.

The cast between them is written out:

```go
Places.Location.Expr().AsGeography()   // metres, on the spheroid
Places.Spot.Expr().AsGeometry()        // the SRID's units, in the plane
```

Nothing casts on your behalf. `ST_Distance` over `geometry(Point,4326)` returns
degrees — a number that looks like an answer and is not one — and this package
will not quietly turn it into metres.

A geography needs a longitude/latitude coordinate system. PostGIS answers *Only
lon/lat coordinate systems are supported in geography* for anything else, and
that error reaches you unchanged.

## Registering the codec

`geometry` and `geography` are extension types, so their OIDs are assigned when
somebody ran `CREATE EXTENSION` and differ per database. Teach every connection:

```go
cfg, err := pgxpool.ParseConfig(dsn)
if err != nil {
    return err
}
cfg.AfterConnect = postgis.Register
pool, err := pgxpool.NewWithConfig(ctx, cfg)
```

Alongside generated enum registration:

```go
cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
    if err := domain.RegisterTypes(ctx, c); err != nil {
        return err
    }
    return postgis.Register(ctx, c)
}
```

`AfterConnect` matters: registration is per connection, so a program that
registered once on one connection works in a test and fails the first time the
pool grows. `postgis.RegisterIfPresent` is the form for a program that runs
against databases with and without PostGIS.

Values cross as EWKB in both directions. There is no path by which a coordinate
becomes text in a statement.

## Declaring a spatial column

The Go type says which storage family the column is. The tag says the rest,
because no Go type carries a number:

```go
//orm:table places
type Place struct {
    ID        int64
    Location  postgis.Geometry  `orm:"pgtype:geometry(Point,4326)"`
    Projected *postgis.Geometry `orm:"pgtype:geometry(Point,3857)"`
    Area      postgis.Geometry  `orm:"pgtype:geometry(MultiPolygon,4326)"`
    Track     *postgis.Geometry `orm:"pgtype:geometry(LineStringZ,4326)"`
    Spot      postgis.Geography `orm:"pgtype:geography(Point,4326)"`
    Sketch    *postgis.Geometry // plain geometry: any shape, any SRID
}
```

The generator emits a descriptor carrying all four facts:

```go
Location: postgis.NewGeomCol[Place](src, "location", 4326, postgis.KindPoint, postgis.XY)
Track:    postgis.NewNullGeomCol[Place](src, "track", 4326, postgis.KindLineString, postgis.XYZ)
Sketch:   postgis.NewNullGeomCol[Place](src, "sketch", 0, postgis.AnyKind, postgis.XY)
```

Reconciliation checks the tag against the column and reports the four mismatches
separately: wrong family, wrong shape, wrong SRID, wrong dimensionality. A field
with no tag is checked on its family alone, which is what database-first mode
needs.

The shape is **not** in the Go type. A `geometry(Polygon,4326)` column reads into
a `postgis.Geometry` that happens to be a polygon, because that is what PostGIS
hands back — and because `ST_Intersection` of two polygons is not always one.

## SRID

An SRID says which coordinate system the numbers are in. The same pair of
numbers is a different place on Earth in 4326 and in 3857.

**`SetSRID` relabels. `Transform` reprojects.** They are different operations
with different names:

```go
p := postgis.Value[E](postgis.NewPoint(4326, 10, 20))

p.SetSRID(3857)   // still (10, 20); now claimed to be metres from the origin
p.Transform(3857) // about (1113195, 2273031): the same place, reprojected
```

Confusing them relocates every row silently. A permanent differential test
asserts the two produce different coordinates.

`Transform` needs a source coordinate system, so an expression whose SRID is
unknown is refused rather than guessed at.

Relating geometries in two coordinate systems is refused while the query is
being built, with both SRIDs named — as early as it can be caught, since a
column states its SRID in a type modifier and a Go type cannot carry a number:

```go
postgis.Of(Places.Location).Intersects(postgis.Compose(Places.Projected.Expr()))
// error: this relates a geometry in SRID 4326 to one in SRID 3857
```

SRID 0 means *no coordinate system stated*. It is not 4326, nothing infers 4326,
and an expression with no SRID is not checked — the server remains the
authority and refuses it there.

## Dimensionality

PostGIS keeps four, and all four survive end to end — value, EWKB, storage,
projection, `COPY`, `RETURNING`:

| | ordinates | declared as |
|---|---|---|
| `postgis.XY` | x, y | `geometry(Point,4326)` |
| `postgis.XYZ` | x, y, z | `geometry(PointZ,4326)` |
| `postgis.XYM` | x, y, m | `geometry(PointM,4326)` |
| `postgis.XYZM` | x, y, z, m | `geometry(PointZM,4326)` |

Z is an elevation and M is a measure; they are independent, and nothing here
reads one as the other. `ST_MakePoint` with three arguments means Z, so
`postgis.MakePointM` is a separate constructor over `ST_MakePointM`.

Every part of one geometry has the same dimensionality. PostGIS answers
*Dimensions mismatch in lwcollection* for bytes that say otherwise, and the
decoder refuses them too.

`Force2D` and `Force3D` change it, and they are spelled out because they lose
data.

## NULL and EMPTY

They are different and stay different:

- a **NULL** column has no geometry — it reads back as a nil pointer
- an **empty** geometry is a geometry that covers nothing — `POLYGON EMPTY` is a
  polygon, it reads back as a value, and `IsEmpty` is true for it

Everything strict follows SQL: `ST_Area(NULL)` is NULL, `ST_AsText(NULL)` is
NULL, a predicate against NULL is UNKNOWN and does not match. Over an empty
geometry the same functions return values — `ST_Area` is 0, `ST_AsText` is
`POLYGON EMPTY` — which is why collapsing the two would lose an answer.

An empty geometry does not intersect itself, and `POINT EMPTY` is not
`POINT(0 0)`.

## Predicates

The OGC set is there, and the pairs that look alike are not alike:

| | includes the boundary |
|---|---|
| `Contains` | no |
| `Covers` | yes |
| `Within` | no |
| `CoveredBy` | yes |

A point on the edge of a zone is in the zone by any reasonable reading, and
`ST_Contains` says it is not. That is the distinction that silently loses an
order, and the differential corpus separates every such pair.

`Crosses` requires passing through the interior: a line wholly inside a polygon
intersects it and does not cross it. `Touches` is true where `Overlaps` is
false. `EqualsGeom` is topological — a polygon written backwards with a repeated
vertex equals the one without — which is a different question from the `Eq` the
column descriptor inherits, and a different question again from
`Geometry.Equal` in Go, which compares structure.

`Relate` takes a DE-9IM pattern, validated against a closed alphabet and then
bound as a parameter.

The bounding-box operators (`&&`, `~`, `@`, `~=`) are exposed on their own
because sometimes the box is the question. They are not substitutes for the
exact predicates.

## Distances and units

```go
Places.Spot.Expr().Distance(...)      // metres, on the spheroid
Places.Location.Expr().Distance(...)  // the SRID's units — degrees for 4326
```

`DWithin` reaches the server as `ST_DWithin`, never rewritten to
`ST_Distance(...) < d`. The two select the same rows and plan completely
differently: PostGIS expands a bounding box for the first and cannot use an
index for the second.

`KNNDistance` builds the `<->` operator, which is what lets PostgreSQL walk a
GiST index in distance order and stop after ten. **Its number is not
`ST_Distance`.** Over geography it measures on a sphere where `ST_Distance`
measures on the spheroid; the two differ by about a part in a thousand. Order by
it; do not report it as the distance.

Measurements read back as the Go types PostgreSQL actually returns, checked
against `pg_typeof`:

| | PostgreSQL | Go |
|---|---|---|
| `Area`, `Length`, `Distance` | `double precision` | `float64` |
| `SRID`, `NumPoints`, `Dimension` | `integer` | `int32` |
| `CoordDim` | `smallint` | `int16` |
| `GeometryType`, `AsText`, `AsEWKT`, `AsGeoJSON` | `text` | `string` |
| `AsBinary`, `AsEWKB` | `bytea` | `[]byte` |
| `Extent` | `box2d` | `postgis.Box2D` |

`ST_AsGeoJSON` returns **text**, not `jsonb`. It reads back as a Go `string`.

`X`, `Y`, `Z` and `M` read back as pointers. They are NULL when the geometry is
NULL, and NULL when the geometry carries no such ordinate — `ST_Z` of an XY
point. They **raise** when the geometry is not a point: PostGIS answers
*Argument to ST_X() must have type POINT*, and that error reaches you.

## Shapes that change

Several functions return a shape that depends on their input, so the result
claims none:

| | can return |
|---|---|
| `Envelope` | point, line or polygon |
| `ConvexHull` | point, line or polygon |
| `Intersection` | polygon, line, point or empty |
| `Union` | polygon, multi-polygon or collection |
| `Difference`, `SymDifference` | any of them |
| `MakeValid` | a different shape and multiplicity |
| `Buffer` | polygon, multi-polygon, or empty for a negative distance |

The envelope of a point is a point. The intersection of two polygons meeting
along an edge is a line. Typing any of these as `Polygon` would be a promise the
database breaks, and a test asserts each one against the server.

Where PostGIS does guarantee a shape it is claimed: `Centroid` and
`PointOnSurface` are points, `MakeEnvelope` is a polygon.

`MakeValid` is never applied on your behalf. An invalid geometry is data
somebody should look at.

## Reading and writing

Spatial values go through the ordinary machinery: `Insert`, `Update`,
`RETURNING`, `CopyFrom`, typed projections, derived tables, CTEs, correlated
subqueries, `CASE`, `LATERAL`, window functions.

```go
db.Places.Update().
    Set(Places.Projected.SetExpr(
        orm.Nullable(Places.Location.Expr().Transform(3857).Value()))).
    Where(Places.Name.Eq("depot")).
    Exec(ctx)
```

A geometry projected out of a derived table or CTE is an ordinary
`orm.Expression`. `postgis.FromExpr` brings it back into the spatial layer, and
the caller states what it holds — the projection did not carry that, and nothing
checks the claim:

```go
loc  := orm.Named("location", orm.Of(Places.Location))
near := orm.CTE("near", orm.Rows(loc).From(Places.Source()))

postgis.Of(Zones.Area).Covers(
    postgis.FromExpr(orm.Ref(near, loc), Places.Location.TypeMod()))
```

The three aggregates PostGIS actually has are `Collect`, `UnionAgg` and
`Extent`. All are nullable, because an aggregate over an empty group is NULL —
`ST_Extent` over no rows is NULL and not an empty box.

Outer joins work as they do everywhere: a NOT NULL geography read through a
`LEFT JOIN` is nullable, and so is every measurement, transformation and
rendering derived from it. Reading one into a destination that cannot hold NULL
is refused.

## Indexes

Use the generic index model:

```sql
CREATE INDEX places_location_gist ON places USING gist (location);
CREATE INDEX places_spot_gist     ON places USING gist (spot);
```

These round-trip with no drift, on both storage families, with partial
predicates and with a non-default operator class such as
`gist_geometry_ops_nd`.

PostgreSQL omits the **default** operator class from `pg_get_indexdef`, so a
desired schema that spelled out `gist_geometry_ops_2d` would differ from the
catalog on every run. That is true of every type, not something spatial: declare
the default by leaving it out.

An index never changes which rows come back. The same corpus is asserted with
the index, without it, and after dropping and recreating it.

## Migrations

Spatial columns are ordinary columns to the migration engine, and their type
modifier is what makes them different columns. Adding, dropping and changing
nullability all round-trip with no drift, and the historical state preserves the
family, the shape, the SRID and the dimensionality.

`CREATE EXTENSION postgis` is a declaration like any other. Nothing installs it
because a spatial column appeared; a schema that uses geometry without declaring
the extension produces a migration that fails at the server.

Three type changes are dangerous and the engine invents no conversion for any of
them:

| change | what happens |
|---|---|
| `geometry(Point,4326)` → `geometry(Point,3857)` | PostgreSQL refuses: the existing rows fail the new type modifier |
| `geometry(Point,…)` → `geometry(Polygon,…)` | PostgreSQL refuses and asks for a `USING` clause |
| `geography` → `geometry` | PostgreSQL refuses and asks for a `USING` clause |
| **`geometry` → `geography`** | **PostgreSQL accepts it silently** |

The last one is the dangerous one. PostGIS defines that cast as an assignment
cast, so the `ALTER` succeeds, no row is rejected, and not one coordinate
changes — but every distance, length and area over the column silently becomes
metres on a spheroid instead of units in a plane. The engine emits **W212** for
it, because nothing else will catch it. Transform the coordinates first if that
is what was meant.

## What is not supported

Deliberately absent, and not planned as part of this milestone:

- **Curved and surface types** — `CircularString`, `CompoundCurve`,
  `CurvePolygon`, `MultiCurve`, `MultiSurface`, `PolyhedralSurface`, `TIN`,
  `Triangle`. The decoder rejects them by name rather than reading their
  coordinates into a shape they are not.
- **SP-GiST and BRIN spatial indexes.**
- **A geography aggregate.** PostGIS defines `ST_Collect` and `ST_Union` on
  geometry only; cast with `AsGeometry`, aggregate, and cast the result back if
  that is what is wanted.
- **`ST_AsMVT` and the tile functions.**
- **Type-level SRIDs.** An SRID is a number and a Go type cannot carry one, so
  the check is at query-build time against the column's declared type.
- **Raster and pgRouting.**

## Verified against

| PostgreSQL | PostGIS |
|---|---|
| 17 | 3.5 |
| 16 | 3.4 |
| 14 | 3.4 |
