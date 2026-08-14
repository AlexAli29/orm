---
title: PostGIS
description: Spatial types that stay spatial — geometry and geography, kept apart.
---

## Opt-in, and separate

PostGIS support is its own package. A project that does not import it never sees
a spatial API, and the root ORM knows nothing about geometry:

```go
import "github.com/AlexAli29/orm/postgis"
```

Everything in it composes through the one extension boundary the root package
exposes. There is no second query compiler and no second expression model — a
spatial predicate is an `orm.Predicate` like any other, and it nests into
composed queries, CTEs and derived tables unchanged.

## The two distinctions that are never blurred

**geometry** is Cartesian, in whatever units the SRID's coordinate system uses.
**geography** is on the spheroid, with distances and lengths in metres.

They are different PostgreSQL types with different index behaviour and different
answers, so they are different Go types here. Converting between them is
something you write, not something that happens to you.

And two facts travel with every value and every column:

- **the shape** — Point, LineString, Polygon, and the multi forms
- **the SRID** — which coordinate system the numbers are in

Losing either is how a query comes to compare metres with degrees and get a
number back.

## Declaring a spatial column

The `pgtype` tag carries the shape and the coordinate system, because neither is
derivable from the Go type:

```go
//orm:table public.places
type Place struct {
    ID   int64  `orm:"pk,identity"`
    Name string

    // On the spheroid. Distances come back in metres.
    Spot postgis.Geography `orm:"pgtype:geography(Point,4326)"`

    // Cartesian, in WGS 84 degrees.
    Location postgis.Geometry `orm:"pgtype:geometry(Point,4326)"`

    // The same place in web Mercator. Relating it to Location without
    // transforming first is a mistake the SRID makes visible.
    Projected *postgis.Geometry `orm:"pgtype:geometry(Point,3857)"`

    Footprint *postgis.Geometry `orm:"pgtype:geometry(Polygon,4326)"`
}
```

A pointer is a nullable column, as everywhere else. The generator emits
`GeomCol`, `GeogCol` and their nullable forms, each carrying the SRID, the kind
and the dimension it was declared with.

## Querying

`postgis.Of` lifts a geometry column into a spatial expression;
`postgis.OfGeog` does the same for geography:

```go
// Everything within 5 km of a point, on the spheroid — metres, because
// geography measures in metres.
here := postgis.GeographyPoint(-0.1276, 51.5072)

places, err := db.Places.Query().
    Where(postgis.OfGeog(Places.Spot).
        DWithin(postgis.GeogValue[Place](here), 5000)).
    All(ctx)
```

```go
// Cartesian relationships, on geometry.
db.Places.Query().Where(postgis.Of(Places.Location).Intersects(v))
db.Places.Query().Where(postgis.Of(Places.Location).Within(v))
db.Places.Query().Where(postgis.Of(Places.Location).Contains(v))
```

### Bounding-box operators are named as such

```go
postgis.Of(Places.Location).BBoxIntersects(v)  // &&
postgis.Of(Places.Location).BBoxContains(v)    // ~
postgis.Of(Places.Location).BBoxWithin(v)      // @
```

`&&` is not `ST_Intersects`. It compares bounding boxes, which is cheaper and
answers a different question — so it gets a different name rather than being
presented as a faster version of the exact one.

## Measurements and transformations

These return ordinary `orm.Value`, so they go in projections and orderings like
anything else:

```go
distance := postgis.OfGeog(Places.Spot).Distance(postgis.GeogValue[Place](here))

type Near struct {
    Name   string
    Metres float64
}

var near = orm.Project2(
    Places.Name, distance,
    func(name string, m float64) Near { return Near{name, m} },
)

rows, err := orm.Select(db.Places, near).
    OrderBy(distance.Asc()).
    Limit(20).
    All(ctx)
```

Also available on an expression: `Area`, `Length`, `Centroid`, `Buffer`,
`Boundary`, `Azimuth`, `AsText`, `AsEWKT`, `AsGeoJSON`, `AsBinary`, `AsEWKB`,
and `AsGeography` for the conversion you write deliberately. Each has a `…Null`
form for the nullable column, because a measurement of a NULL geometry is NULL.

## Aggregates

```go
postgis.Collect(g)   // ST_Collect   -> *Geometry
postgis.UnionAgg(g)  // ST_Union     -> *Geometry
postgis.Extent(g)    // ST_Extent    -> *Box2D
postgis.Extent3D(g)  // ST_3DExtent  -> *Box3D
```

## Registering the types

pgx needs to be told about the PostGIS types on each connection:

```go
cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
    return postgis.Register(ctx, conn)
}
```

`RegisterIfPresent` is the tolerant form — it reports whether the extension was
there rather than failing, which is what a binary that runs against both spatial
and plain databases wants.

## Versions this is proved against

PostgreSQL 17 with PostGIS 3.5, 16 with 3.4, and 14 with 3.4.

The spatial suite skips when the extension is unavailable, which is right on a
developer's machine and wrong in CI — so CI sets `ORM_REQUIRE_POSTGIS=1`, which
turns the skip into a failure. A support claim nothing exercises is a claim
nobody should believe.

The ORM never creates the extension. `CREATE EXTENSION postgis` is a privileged
operation belonging to whoever owns the database.

## Worked examples

### Shops near me

```go
here := postgis.GeographyPoint(lon, lat)

type Near struct {
    Name   string
    Metres float64
}

distance := postgis.OfGeog(Shops.Spot).Distance(postgis.GeogValue[Shop](here))

var near = orm.Project2(
    Shops.Name, distance,
    func(name string, m float64) Near { return Near{name, m} },
)

rows, err := orm.Select(db.Shops, near).
    Where(postgis.OfGeog(Shops.Spot).DWithin(postgis.GeogValue[Shop](here), 2000)).
    OrderBy(distance.Asc()).
    Limit(10).
    All(ctx)
```

`DWithin` before `Distance` matters: the first can use a spatial index, the
second cannot. Filtering then sorting is the difference between a query and a
full scan.

### Which delivery zone covers an address

```go
zone, err := db.Zones.Query().
    Where(postgis.Of(Zones.Area).Contains(postgis.Of(Addresses.Point))).
    One(ctx)
```

### A bounding box for a map viewport

```go
box := postgis.MakeEnvelope(west, south, east, north, 4326)

pins, err := db.Pins.Query().
    Where(postgis.Of(Pins.Location).BBoxIntersects(box)).
    Limit(500).
    All(ctx)
```

`BBoxIntersects` is `&&`, which compares bounding boxes. For a rectangular
viewport that is the exact question, and it is the cheap one.

### Exporting for a map client

```go
var geo = orm.Project2(
    Zones.Name, postgis.Of(Zones.Area).AsGeoJSON(),
    func(name, geom string) Feature { return Feature{name, geom} },
)
```

### Registering the types

```go
cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
    return postgis.Register(ctx, conn)
}
```
