package postgis

import (
	"fmt"

	"github.com/AlexAli29/orm"
)

// Measurements and accessors.
//
// Every one of these is a value expression, so it goes in a select list, an
// ORDER BY, a HAVING, or the right-hand side of an assignment, and it scans
// through M10's typed projections with no reflection. What this layer decides
// is the Go type each one reads back as, and it decides it from what PostgreSQL
// actually returns rather than from what the name suggests:
//
//	ST_Area, ST_Length, ST_Distance   double precision      float64
//	ST_SRID, ST_NPoints, ST_Dimension integer               int32
//	ST_CoordDim                       smallint              int16
//	ST_GeometryType                   text                  string
//
// The tests check every one of these against pg_typeof rather than against this
// comment, because a claim about a result type that nobody asked the server
// about is a guess.
//
// Nullability has two sources and both are honoured. Some of these are NULL for
// a reason inside the geometry — ST_Z of a geometry with no Z ordinate,
// ST_Azimuth of two points in the same place — and those return the nullable
// form always. The rest are strict: NULL in, NULL out, so a
// measurement over a column that can be NULL is nullable and one over a column
// that cannot is not.
//
// A column descriptor knows which of the two it is, so the methods on the four
// descriptors have the right static type with nothing to decide. An expression
// built by chaining does not — GeomExpr is one Go type either way — so it
// offers both forms and refuses the wrong one while the query is being built,
// with a message naming the other. That is the boundary of what Go's type
// system reaches here, and it is drawn where a wrong answer would otherwise be
// possible rather than hidden.

// nullMismatch is the refusal that keeps a nullable expression from being read
// into a destination that cannot hold NULL.
func nullMismatch(fn, alternative string) error {
	return fmt.Errorf("postgis: %s here is over an expression that can be NULL,"+
		" so its result can be NULL and would not fit a destination that cannot hold one;"+
		" use %s, which reads back as a pointer", fn, alternative)
}

// notNullMismatch is the opposite refusal: asking for the nullable form of
// something that cannot produce NULL would make every caller check for a NULL
// that never comes.
func notNullMismatch(fn, alternative string) error {
	return fmt.Errorf("postgis: %s here is over an expression that cannot be NULL,"+
		" so its result cannot be either; use %s", fn, alternative)
}

// measure builds a float8 measurement whose result cannot be NULL.
func (g GeomExpr[E]) measure(fn, alternative string, extra ...orm.Arg) orm.Value[E, float64] {
	if g.nullable {
		return orm.Fn[E, float64](fn, orm.ArgFail(nullMismatch(fn, alternative)))
	}
	return orm.Fn[E, float64](fn, append([]orm.Arg{g.arg}, extra...)...)
}

// measureNull builds a float8 measurement whose result can be NULL.
func (g GeomExpr[E]) measureNull(fn, alternative string, extra ...orm.Arg) orm.Value[E, *float64] {
	if !g.nullable {
		return orm.FnNull[E, float64](fn, orm.ArgFail(notNullMismatch(fn, alternative)))
	}
	return orm.FnNull[E, float64](fn, append([]orm.Arg{g.arg}, extra...)...)
}

// Area is the geometry's area in the units of its coordinate system.
//
// Over geometry(4326) that is square degrees, which is not an area anybody
// wants: a square degree is not a fixed size. Cast to geography, or use a
// projected coordinate system, when square metres are meant.
func (g GeomExpr[E]) Area() orm.Value[E, float64] { return g.measure("ST_Area", "AreaNull") }

// AreaNull is [GeomExpr.Area] over a geometry that can be NULL.
func (g GeomExpr[E]) AreaNull() orm.Value[E, *float64] {
	return g.measureNull("ST_Area", "Area")
}

// Length is the geometry's length in the units of its coordinate system. It is
// zero for anything that is not a line.
func (g GeomExpr[E]) Length() orm.Value[E, float64] { return g.measure("ST_Length", "LengthNull") }

// LengthNull is [GeomExpr.Length] over a geometry that can be NULL.
func (g GeomExpr[E]) LengthNull() orm.Value[E, *float64] {
	return g.measureNull("ST_Length", "Length")
}

// Perimeter is the length of a polygon's boundary, and zero for anything else.
func (g GeomExpr[E]) Perimeter() orm.Value[E, float64] {
	return g.measure("ST_Perimeter", "PerimeterNull")
}

// PerimeterNull is [GeomExpr.Perimeter] over a geometry that can be NULL.
func (g GeomExpr[E]) PerimeterNull() orm.Value[E, *float64] {
	return g.measureNull("ST_Perimeter", "Perimeter")
}

// Distance is the shortest distance between the two geometries, in the units of
// their coordinate system.
//
// It is zero when they touch. Over geometry(4326) the answer is in degrees; see
// [GeogExpr.Distance] for metres.
func (g GeomExpr[E]) Distance(other GeomExpr[E]) orm.Value[E, float64] {
	if err := g.distanceError(other); err != nil {
		return orm.Fn[E, float64]("ST_Distance", orm.ArgFail(err))
	}
	if g.nullable || other.nullable {
		return orm.Fn[E, float64]("ST_Distance", orm.ArgFail(nullMismatch("ST_Distance", "DistanceNull")))
	}
	return orm.Fn[E, float64]("ST_Distance", g.arg, other.arg)
}

// DistanceNull is [GeomExpr.Distance] where either geometry can be NULL, and is
// nullable because PostgreSQL's answer then is NULL rather than zero — a
// distinction that matters, because zero means "they touch".
func (g GeomExpr[E]) DistanceNull(other GeomExpr[E]) orm.Value[E, *float64] {
	if err := g.distanceError(other); err != nil {
		return orm.FnNull[E, float64]("ST_Distance", orm.ArgFail(err))
	}
	if !g.nullable && !other.nullable {
		return orm.FnNull[E, float64]("ST_Distance",
			orm.ArgFail(notNullMismatch("ST_Distance", "Distance")))
	}
	return orm.FnNull[E, float64]("ST_Distance", g.arg, other.arg)
}

func (g GeomExpr[E]) distanceError(other GeomExpr[E]) error {
	return checkSRID(g, other, "ST_Distance")
}

// MaxDistance is the greatest distance between any point of one geometry and
// any point of the other.
func (g GeomExpr[E]) MaxDistance(other GeomExpr[E]) orm.Value[E, float64] {
	if err := g.distanceError(other); err != nil {
		return orm.Fn[E, float64]("ST_MaxDistance", orm.ArgFail(err))
	}
	if g.nullable || other.nullable {
		return orm.Fn[E, float64]("ST_MaxDistance",
			orm.ArgFail(nullMismatch("ST_MaxDistance", "an outer join-safe form")))
	}
	return orm.Fn[E, float64]("ST_MaxDistance", g.arg, other.arg)
}

// KNNDistance builds the <-> operator, which is what an ORDER BY uses to find
// the nearest rows through a spatial index.
//
// This is the one to reach for when the question is "the ten closest", because
// PostgreSQL can walk a GiST index in distance order and stop after ten. An
// ORDER BY ST_Distance cannot: it has to measure every row first.
//
//	q.OrderBy(places.Location.Expr().KNNDistance(here).Asc()).Limit(10)
//
// The number it returns is a distance between the geometries' centroids in
// recent PostGIS and between their bounding boxes in older ones. Order by it;
// do not report it as the distance.
func (g GeomExpr[E]) KNNDistance(other GeomExpr[E]) orm.Value[E, float64] {
	if err := g.distanceError(other); err != nil {
		return orm.Fn[E, float64]("ST_Distance", orm.ArgFail(err))
	}
	return orm.Op[E, float64]("<->", g.arg, other.arg)
}

// BBoxDistance builds the <#> operator: the distance between the two
// geometries' bounding boxes.
func (g GeomExpr[E]) BBoxDistance(other GeomExpr[E]) orm.Value[E, float64] {
	if err := g.distanceError(other); err != nil {
		return orm.Fn[E, float64]("ST_Distance", orm.ArgFail(err))
	}
	return orm.Op[E, float64]("<#>", g.arg, other.arg)
}

// The ordinate accessors.
//
// All four read back as pointers, and the two reasons they can be NULL are
// worth keeping apart because only one of them is about the value.
//
// A NULL geometry gives a NULL ordinate, because these are strict like
// everything else. That is the reason the result type is a pointer even over a
// NOT NULL column read through an outer join.
//
// A geometry that has no such ordinate also gives NULL: ST_Z of an XY point is
// NULL, and so is ST_M of an XYZ point. That is PostGIS's answer, and it is
// checked against the server in the type matrix rather than assumed.
//
// A geometry that is not a point is a third case, and it is not NULL: PostGIS
// raises "Argument to ST_X() must have type POINT" on every supported version.
// These do not catch it, convert it, or pretend the answer is NULL — the error
// is PostgreSQL's and reaches the caller as one. Nothing in this package's type
// system prevents asking, because the shape a column holds is a type modifier
// rather than a Go type; a column declared geometry(Point,4326) is the way to
// know the question is well formed.

// X is the point's first ordinate.
//
// It is NULL when the geometry is NULL, and an error when the geometry is not a
// point.
func (g GeomExpr[E]) X() orm.Value[E, *float64] { return orm.FnNull[E, float64]("ST_X", g.arg) }

// Y is the point's second ordinate, with the same two cases [GeomExpr.X] has.
func (g GeomExpr[E]) Y() orm.Value[E, *float64] { return orm.FnNull[E, float64]("ST_Y", g.arg) }

// Z is the point's elevation.
//
// It is NULL when the geometry is NULL and when the geometry carries no Z —
// an XY or XYM point — and an error when the geometry is not a point.
func (g GeomExpr[E]) Z() orm.Value[E, *float64] { return orm.FnNull[E, float64]("ST_Z", g.arg) }

// M is the point's measure, with the same cases [GeomExpr.Z] has: NULL for a
// point carrying no measure, an error for something that is not a point.
func (g GeomExpr[E]) M() orm.Value[E, *float64] { return orm.FnNull[E, float64]("ST_M", g.arg) }

// Azimuth is the bearing from one point to another, in radians clockwise from
// north, and NULL when the two points are in the same place.
func (g GeomExpr[E]) Azimuth(other GeomExpr[E]) orm.Value[E, *float64] {
	if err := g.distanceError(other); err != nil {
		return orm.FnNull[E, float64]("ST_Azimuth", orm.ArgFail(err))
	}
	return orm.FnNull[E, float64]("ST_Azimuth", g.arg, other.arg)
}

// The integer accessors. PostGIS returns integer rather than bigint for all of
// them, so they read back as int32 — a claim the differential tests check
// against pg_typeof rather than take on trust.

// SRID is the coordinate system the stored geometry is labelled with.
//
// It is the server's answer rather than the column's declaration, which is why
// it exists: a plain geometry column can hold geometries in several coordinate
// systems, and this is how a query finds out which.
func (g GeomExpr[E]) SRID() orm.Value[E, int32] { return g.count("ST_SRID") }

// NumPoints is how many positions the geometry holds, across every ring and
// every member.
func (g GeomExpr[E]) NumPoints() orm.Value[E, int32] { return g.count("ST_NPoints") }

// NumGeometries is how many members a collection or multi geometry has, and one
// for a simple geometry.
func (g GeomExpr[E]) NumGeometries() orm.Value[E, int32] { return g.count("ST_NumGeometries") }

// Dimension is the geometry's topological dimension: zero for a point, one for
// a line, two for a polygon.
func (g GeomExpr[E]) Dimension() orm.Value[E, int32] { return g.count("ST_Dimension") }

// CoordDim is how many ordinates each position carries: two, three or four.
//
// It reads back as int16 because PostGIS returns smallint for this one and
// integer for every other counting function beside it. Claiming int32 would
// compile, scan and be wrong about the type the server sent — which is the
// reason every one of these is checked against pg_typeof rather than guessed
// from the name.
func (g GeomExpr[E]) CoordDim() orm.Value[E, int16] {
	if g.nullable {
		return orm.Fn[E, int16]("ST_CoordDim", orm.ArgFail(nullMismatch("ST_CoordDim",
			"the nullable form on the column descriptor")))
	}
	return orm.Fn[E, int16]("ST_CoordDim", g.arg)
}

func (g GeomExpr[E]) count(fn string) orm.Value[E, int32] {
	if g.nullable {
		return orm.Fn[E, int32](fn, orm.ArgFail(nullMismatch(fn,
			"the nullable form on the column descriptor")))
	}
	return orm.Fn[E, int32](fn, g.arg)
}

// GeometryType is the shape's name as PostGIS spells it: ST_Point, ST_Polygon,
// and so on, with the ST_ prefix that GeometryType does not have.
func (g GeomExpr[E]) GeometryType() orm.Value[E, string] {
	if g.nullable {
		return orm.Fn[E, string]("ST_GeometryType",
			orm.ArgFail(nullMismatch("ST_GeometryType", "the nullable form on the column descriptor")))
	}
	return orm.Fn[E, string]("ST_GeometryType", g.arg)
}

// Geography measurements, which are the reason the type exists.
//
// Every one of these is in metres — or square metres — on the spheroid,
// whatever the latitude and whatever the coordinate system's units would have
// been. That is a different number from the plane form and usually the one that
// was wanted.

// Distance is the shortest distance between the two geographies, in metres.
func (g GeogExpr[E]) Distance(other GeogExpr[E]) orm.Value[E, float64] {
	return g.e.Distance(other.e)
}

// DistanceNull is [GeogExpr.Distance] where either side can be NULL.
func (g GeogExpr[E]) DistanceNull(other GeogExpr[E]) orm.Value[E, *float64] {
	return g.e.DistanceNull(other.e)
}

// Area is the geography's area in square metres.
func (g GeogExpr[E]) Area() orm.Value[E, float64] { return g.e.Area() }

// AreaNull is [GeogExpr.Area] over a geography that can be NULL.
func (g GeogExpr[E]) AreaNull() orm.Value[E, *float64] { return g.e.AreaNull() }

// Length is the geography's length in metres.
func (g GeogExpr[E]) Length() orm.Value[E, float64] { return g.e.Length() }

// LengthNull is [GeogExpr.Length] over a geography that can be NULL.
func (g GeogExpr[E]) LengthNull() orm.Value[E, *float64] { return g.e.LengthNull() }

// Perimeter is the length of a geography polygon's boundary, in metres.
func (g GeogExpr[E]) Perimeter() orm.Value[E, float64] { return g.e.Perimeter() }

// SRID is the coordinate system the stored geography is labelled with.
func (g GeogExpr[E]) SRID() orm.Value[E, int32] { return g.e.count("ST_SRID") }

// KNNDistance builds the <-> operator over geographies, which orders by
// distance on the sphere through a spatial index.
func (g GeogExpr[E]) KNNDistance(other GeogExpr[E]) orm.Value[E, float64] {
	return g.e.KNNDistance(other.e)
}

// Descriptor conveniences.
//
// These are the common path — a measurement straight off a column — and they
// are where Go's types do the whole job: the descriptor knows statically
// whether its column can be NULL, so the result type is settled at compile time
// and there is nothing to refuse.

// Area is the column's area in the units of its coordinate system.
func (c GeomCol[E]) Area() orm.Value[E, float64] { return c.Expr().Area() }

// Area is the column's area, and can be NULL because the column can.
func (c NullGeomCol[E]) Area() orm.Value[E, *float64] { return c.Expr().AreaNull() }

// Length is the column's length in the units of its coordinate system.
func (c GeomCol[E]) Length() orm.Value[E, float64] { return c.Expr().Length() }

// Length is the column's length, and can be NULL because the column can.
func (c NullGeomCol[E]) Length() orm.Value[E, *float64] { return c.Expr().LengthNull() }

// Area is the column's area in square metres on the spheroid.
func (c GeogCol[E]) Area() orm.Value[E, float64] { return c.Expr().Area() }

// Area is the column's area in square metres, and can be NULL because the
// column can.
func (c NullGeogCol[E]) Area() orm.Value[E, *float64] { return c.Expr().AreaNull() }

// Length is the column's length in metres on the spheroid.
func (c GeogCol[E]) Length() orm.Value[E, float64] { return c.Expr().Length() }

// Length is the column's length in metres, and can be NULL because the column
// can.
func (c NullGeogCol[E]) Length() orm.Value[E, *float64] { return c.Expr().LengthNull() }
