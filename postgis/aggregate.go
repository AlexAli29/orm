package postgis

import (
	"github.com/AlexAli29/orm"
)

// Spatial aggregates.
//
// These are ordinary [orm.Agg] values built through the extension boundary, so
// they carry DISTINCT, FILTER, GROUP BY and the HAVING comparisons every other
// aggregate has. There is no spatial aggregate framework, because an aggregate
// that needed one would not compose with the rest of a query.
//
// All three are nullable, and not as a precaution: an aggregate over an empty
// group is NULL in PostgreSQL, and a spatial one is no exception. ST_Extent
// over no rows is NULL rather than an empty box, and the two are different
// answers to "what area do these cover".

// Collect gathers the group's geometries into one multi geometry or collection,
// without dissolving the boundaries between them.
//
// It is much cheaper than [UnionAgg] and answers a different question: Collect
// puts a hundred polygons in one value, Union merges them into the shape they
// jointly cover. Reach for Collect when the parts still matter.
//
// The result's shape depends on what went in — points give a MultiPoint, mixed
// shapes give a GeometryCollection — so it claims none.
func Collect[E any](g GeomExpr[E]) orm.Agg[E, *Geometry] {
	return orm.AggNull[E, Geometry]("ST_Collect", g.arg)
}

// UnionAgg merges the group's geometries into the single shape they cover,
// dissolving shared boundaries.
//
// This is the aggregate ST_Union, which is a different function from the
// two-argument [GeomExpr.Union] despite the shared name — PostgreSQL tells them
// apart by arity and this package tells them apart by which type they hang on.
//
// It is expensive. Merging a large group is a real geometric computation, and a
// query that does it per row of a join will be slow for reasons no index fixes.
func UnionAgg[E any](g GeomExpr[E]) orm.Agg[E, *Geometry] {
	return orm.AggNull[E, Geometry]("ST_Union", g.arg)
}

// Extent is the bounding box of every geometry in the group.
//
// It returns a [Box2D] and not a geometry, because that is what PostGIS
// returns: ST_Extent's result type is box2d, a type with its own text form, no
// SRID and no binary send function. Typing it as a geometry would put the EWKB
// codec in front of bytes that are not EWKB.
//
// The box carries no coordinate system. Use [Box2D.Geometry] with the SRID the
// group was in when a geometry is what is wanted.
func Extent[E any](g GeomExpr[E]) orm.Agg[E, *Box2D] {
	return orm.AggNull[E, Box2D]("ST_Extent", g.arg)
}

// Extent3D is [Extent] over three dimensions, returning a box3d.
func Extent3D[E any](g GeomExpr[E]) orm.Agg[E, *Box3D] {
	return orm.AggNull[E, Box3D]("ST_3DExtent", g.arg)
}

// There is no geography aggregate here.
//
// PostGIS defines ST_Collect and ST_Union on geometry only, so a geography
// aggregate would have to cast down, aggregate, and cast back — and the cast
// back cannot be expressed on an aggregate node without claiming a result type
// the server does not produce. Cast the column with AsGeometry, aggregate, and
// cast the result yourself if that is what is wanted: it is two visible steps
// rather than one hidden one.
