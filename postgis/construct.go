package postgis

import (
	"fmt"

	"github.com/AlexAli29/orm"
)

// Constructors, transformations and set operations.
//
// Two things run through all of them.
//
// The first is that the result's shape is only as narrow as PostGIS actually
// guarantees. ST_Envelope of a degenerate polygon is a point; ST_Intersection
// of two polygons that meet along an edge is a line; ST_Buffer of a zero
// distance is whatever it was given. Typing any of those as Polygon would be a
// promise this package cannot keep, so the result metadata says "any shape" and
// the value that comes back reports what it really is. Where PostGIS does
// guarantee a shape — ST_Centroid is a point, ST_MakePoint is a point — the
// metadata says so.
//
// The second is that nothing transforms coordinates on its own. [SetSRID]
// relabels and [Transform] reprojects, they are different functions with
// different names, and neither happens because an expression would otherwise
// not typecheck. An SRID mismatch is an error, not an invitation.

// MakePoint builds ST_MakePoint from two expressions.
//
// The result has no SRID, because ST_MakePoint does not assign one: PostGIS
// returns a geometry in SRID 0, and a package that quietly made it 4326 would
// put every constructed point somewhere on Earth by accident. Wrap it in
// [GeomExpr.SetSRID] to say which coordinate system the numbers are in.
//
// The ordinates are ordinary value expressions, so a point can be built from
// two columns — which is what a table of latitudes and longitudes needs, and
// the reason this exists alongside the Go-side [NewPoint].
func MakePoint[E any](x, y orm.Selectable[E, float64]) GeomExpr[E] {
	return GeomExpr[E]{
		arg:  orm.ArgOf(orm.Fn[E, Geometry]("ST_MakePoint", orm.ArgOf(x), orm.ArgOf(y))),
		srid: UnknownSRID,
		kind: KindPoint,
	}
}

// MakePointZ builds ST_MakePoint with an elevation.
func MakePointZ[E any](x, y, z orm.Selectable[E, float64]) GeomExpr[E] {
	return GeomExpr[E]{
		arg: orm.ArgOf(orm.Fn[E, Geometry]("ST_MakePoint",
			orm.ArgOf(x), orm.ArgOf(y), orm.ArgOf(z))),
		srid: UnknownSRID,
		kind: KindPoint,
		dim:  XYZ,
	}
}

// MakePointM builds ST_MakePointM, which takes a measure rather than an
// elevation.
//
// It is a different function in PostGIS rather than a fourth argument, because
// ST_MakePoint with three arguments means Z. Getting that wrong stores a
// measure where an elevation belongs and nothing complains.
func MakePointM[E any](x, y, m orm.Selectable[E, float64]) GeomExpr[E] {
	return GeomExpr[E]{
		arg: orm.ArgOf(orm.Fn[E, Geometry]("ST_MakePointM",
			orm.ArgOf(x), orm.ArgOf(y), orm.ArgOf(m))),
		srid: UnknownSRID,
		kind: KindPoint,
		dim:  XYM,
	}
}

// SetSRID labels the geometry with a coordinate system.
//
// It changes the metadata and not one coordinate. A point at (1, 2) with SRID
// set to 3857 is still at (1, 2); it is now claimed to be 1 metre east and 2
// metres north of the web-Mercator origin rather than 1 degree east and 2
// degrees north of Greenwich, which is a different place on Earth for the same
// pair of numbers.
//
// Use it when the coordinates are already right and the label is missing —
// after ST_MakePoint, or over a column somebody loaded without one. Use
// [GeomExpr.Transform] when the coordinates need to move.
func (g GeomExpr[E]) SetSRID(srid int32) GeomExpr[E] {
	out := g
	// The SRID is an integer in a grammar position PostgreSQL accepts a
	// parameter in, so it is bound rather than written. It is validated first
	// all the same: a negative SRID is not one, and saying so here beats a
	// constraint violation three statements later.
	if srid < 0 {
		out.arg = orm.ArgFail(fmt.Errorf("postgis: %d is not an SRID; an SRID is not negative", srid))
		return out
	}
	// The SRID is cast because ST_SetSRID is not the only PostGIS function
	// taking a geometry and a second argument, and an untyped parameter leaves
	// PostgreSQL resolving the overload with nothing to go on. ST_Transform is
	// the one this bites: it has an integer form and a proj4-text form, and the
	// server picks the text one and fails on a number.
	out.arg = orm.ArgOf(orm.Fn[E, Geometry]("ST_SetSRID", g.arg, orm.ArgCast(srid, "int4")))
	out.srid = srid
	return out
}

// Transform reprojects the geometry into another coordinate system.
//
// The coordinates move. PostGIS does the projection using the definitions in
// spatial_ref_sys, which is why nothing here computes one: a projection is a
// large body of geodesy that belongs in PROJ and not in an ORM.
//
// It needs a source coordinate system to project from, so an expression whose
// SRID is unknown is refused rather than guessed at.
func (g GeomExpr[E]) Transform(srid int32) GeomExpr[E] {
	out := g
	switch {
	case srid <= 0:
		out.arg = orm.ArgFail(fmt.Errorf("postgis: %d is not a target SRID for ST_Transform", srid))
		return out
	case g.srid == UnknownSRID:
		out.arg = orm.ArgFail(fmt.Errorf(
			"postgis: ST_Transform needs to know what coordinate system to project from,"+
				" and this expression does not state one; label it with SetSRID(%d-or-whatever-it-is) first"+
				" — SetSRID assigns the label and Transform moves the coordinates, and they are not interchangeable",
			4326))
		return out
	}
	out.arg = orm.ArgOf(orm.Fn[E, Geometry]("ST_Transform", g.arg, orm.ArgCast(srid, "int4")))
	out.srid = srid
	return out
}

// MakeEnvelope builds the rectangle between two corners, which is what a map
// viewport is.
//
// The SRID is required rather than optional. PostGIS's own two-argument form
// defaults to 0, and a viewport in SRID 0 intersects nothing in a 4326 table —
// a query that silently returns no rows rather than failing.
func MakeEnvelope[E any](minX, minY, maxX, maxY float64, srid int32) GeomExpr[E] {
	if srid <= 0 {
		return GeomExpr[E]{
			arg: orm.ArgFail(fmt.Errorf(
				"postgis: ST_MakeEnvelope needs a coordinate system and %d is not one;"+
					" a viewport with no SRID intersects nothing in a table that has one", srid)),
		}
	}
	return GeomExpr[E]{
		arg: orm.ArgOf(orm.Fn[E, Geometry]("ST_MakeEnvelope",
			orm.ArgCast(minX, "float8"), orm.ArgCast(minY, "float8"),
			orm.ArgCast(maxX, "float8"), orm.ArgCast(maxY, "float8"),
			orm.ArgCast(srid, "int4"))),
		srid: srid,
		kind: KindPolygon,
	}
}

// Buffer grows the geometry by distance in every direction, in the units of its
// coordinate system.
//
// The result's shape is deliberately unconstrained. A positive buffer of
// anything is a polygon or a multi-polygon, and a negative buffer of a polygon
// can collapse to an empty geometry — so the metadata claims nothing and the
// value reports what it is.
func (g GeomExpr[E]) Buffer(distance float64) GeomExpr[E] {
	return g.derive("ST_Buffer", AnyKind, orm.ArgValue(distance))
}

// BufferMetres grows a geography by distance in metres.
//
// PostGIS returns a geography here, having done the work on the spheroid, which
// is what makes "everything within 500 metres" a question with one answer
// rather than one per latitude.
func (g GeogExpr[E]) BufferMetres(metres float64) GeogExpr[E] {
	out := g.e.derive("ST_Buffer", AnyKind, orm.ArgValue(metres))
	return GeogExpr[E]{e: out}
}

// Centroid is the geometry's centre of mass, which PostGIS guarantees is a
// point — so the result metadata says Point, and that is one of the few places
// it can.
//
// The centroid of an empty geometry is an empty point, and the centroid of a
// NULL is NULL. They are different answers and both come back.
func (g GeomExpr[E]) Centroid() GeomExpr[E] {
	return g.derive("ST_Centroid", KindPoint)
}

// PointOnSurface is a point guaranteed to lie on the geometry, which the
// centroid of a crescent is not.
func (g GeomExpr[E]) PointOnSurface() GeomExpr[E] {
	return g.derive("ST_PointOnSurface", KindPoint)
}

// Envelope is the geometry's bounding box as a geometry.
//
// It is not always a polygon, which is why the result claims no shape: the
// envelope of a point is a point, the envelope of a horizontal line is a line,
// and only the general case is a rectangle. A package that typed this Polygon
// would be wrong about two of the three.
func (g GeomExpr[E]) Envelope() GeomExpr[E] {
	return g.derive("ST_Envelope", AnyKind)
}

// ConvexHull is the smallest convex geometry containing this one.
//
// Like [GeomExpr.Envelope] it can be a point, a line or a polygon depending on
// what it was given, so it claims no shape.
func (g GeomExpr[E]) ConvexHull() GeomExpr[E] {
	return g.derive("ST_ConvexHull", AnyKind)
}

// Boundary is the geometry's boundary: the ring of a polygon, the endpoints of
// a line.
func (g GeomExpr[E]) Boundary() GeomExpr[E] {
	return g.derive("ST_Boundary", AnyKind)
}

// Intersection is the part the two geometries have in common.
//
// The shape depends entirely on how they meet — two polygons crossing give a
// polygon, two polygons touching along an edge give a line, two touching at a
// corner give a point, and two that miss give an empty geometry — so the
// result claims none.
func (g GeomExpr[E]) Intersection(other GeomExpr[E]) GeomExpr[E] {
	return g.combine("ST_Intersection", other)
}

// Union is the two geometries together, as one.
//
// This is the two-argument ST_Union, which is a different function from the
// aggregate of the same name: this combines two values in a row, and
// [UnionAgg] combines every row of a group.
func (g GeomExpr[E]) Union(other GeomExpr[E]) GeomExpr[E] {
	return g.combine("ST_Union", other)
}

// Difference is the part of the receiver that is not in the other geometry.
func (g GeomExpr[E]) Difference(other GeomExpr[E]) GeomExpr[E] {
	return g.combine("ST_Difference", other)
}

// SymDifference is the part of either geometry that is not in both.
func (g GeomExpr[E]) SymDifference(other GeomExpr[E]) GeomExpr[E] {
	return g.combine("ST_SymDifference", other)
}

// Simplify removes vertices using Douglas-Peucker, keeping the result within
// tolerance of the original.
//
// It can break topology: two polygons that shared a boundary may not afterwards,
// and a polygon can simplify into something invalid. That is the algorithm
// rather than an implementation detail, and [GeomExpr.SimplifyPreserveTopology]
// is the slower function that does not do it.
func (g GeomExpr[E]) Simplify(tolerance float64) GeomExpr[E] {
	return g.derive("ST_Simplify", AnyKind, orm.ArgValue(tolerance))
}

// SimplifyPreserveTopology removes vertices while keeping the result valid and
// keeping components that would otherwise vanish.
//
// It is not [GeomExpr.Simplify] with a flag: it is a different algorithm with a
// different cost and a different result, and the two are offered separately
// because choosing between them is a real decision.
func (g GeomExpr[E]) SimplifyPreserveTopology(tolerance float64) GeomExpr[E] {
	return g.derive("ST_SimplifyPreserveTopology", AnyKind, orm.ArgValue(tolerance))
}

// MakeValid repairs an invalid geometry.
//
// The shape can change — a self-intersecting polygon becomes a multi-polygon,
// and a degenerate one can become a line — so the result claims none. Nothing
// calls this on your behalf: an invalid geometry is data somebody should look
// at, and quietly repairing it on the way through a query would hide that.
func (g GeomExpr[E]) MakeValid() GeomExpr[E] {
	return g.derive("ST_MakeValid", AnyKind)
}

// IsValidReason explains why a geometry is invalid, in PostGIS's words.
//
// It returns "Valid Geometry" for a valid one rather than NULL, which is why
// the result is a plain string.
func (g GeomExpr[E]) IsValidReason() orm.Value[E, string] {
	if g.nullable {
		return orm.Fn[E, string]("ST_IsValidReason",
			orm.ArgFail(nullMismatch("ST_IsValidReason", "IsValidReasonNull")))
	}
	return orm.Fn[E, string]("ST_IsValidReason", g.arg)
}

// IsValidReasonNull is [GeomExpr.IsValidReason] over a geometry that can be
// NULL.
func (g GeomExpr[E]) IsValidReasonNull() orm.Value[E, *string] {
	if !g.nullable {
		return orm.FnNull[E, string]("ST_IsValidReason",
			orm.ArgFail(notNullMismatch("ST_IsValidReason", "IsValidReason")))
	}
	return orm.FnNull[E, string]("ST_IsValidReason", g.arg)
}

// Force2D drops the Z and M ordinates.
//
// It is spelled out because it loses data. A column typed geometry(PointZ,4326)
// does not accept the result, and this package will not do it implicitly to
// make an assignment typecheck.
func (g GeomExpr[E]) Force2D() GeomExpr[E] {
	out := g.derive("ST_Force2D", g.kind)
	out.dim = XY
	return out
}

// Force3D adds a Z ordinate of zero where there is none.
func (g GeomExpr[E]) Force3D() GeomExpr[E] {
	out := g.derive("ST_Force3D", g.kind)
	out.dim = XYZ
	return out
}

// Multi wraps a geometry in its multi form, which is what a column typed
// geometry(MultiPolygon,4326) requires of a polygon.
func (g GeomExpr[E]) Multi() GeomExpr[E] {
	return g.derive("ST_Multi", multiKind(g.kind))
}

func multiKind(k Kind) Kind {
	switch k {
	case KindPoint:
		return KindMultiPoint
	case KindLineString:
		return KindMultiLineString
	case KindPolygon:
		return KindMultiPolygon
	default:
		return AnyKind
	}
}

// CollectionExtract pulls the members of one shape out of a collection, which
// is how the polygon part of an ST_Intersection result is isolated.
func (g GeomExpr[E]) CollectionExtract(k Kind) GeomExpr[E] {
	if k != KindPoint && k != KindLineString && k != KindPolygon {
		out := g
		out.arg = orm.ArgFail(fmt.Errorf(
			"postgis: ST_CollectionExtract takes Point, LineString or Polygon, and %s is not one of them", k))
		return out
	}
	// The type code is PostGIS's own numbering, which this package's Kind
	// constants already are; it is a value in a parameter position.
	out := g.derive("ST_CollectionExtract", multiKind(k), orm.ArgCast(int32(k), "int4"))
	return out
}

// derive builds a geometry-valued function over one operand, carrying
// nullability through and stating what shape the result is known to have.
//
// Nullability is inherited rather than reset. Every one of these functions is
// strict, so a NULL geometry produces a NULL result — and an expression that
// claimed otherwise would be read into a destination that cannot hold it.
func (g GeomExpr[E]) derive(fn string, kind Kind, extra ...orm.Arg) GeomExpr[E] {
	args := append([]orm.Arg{g.arg}, extra...)
	out := g
	out.kind = kind
	out.arg = orm.ArgOf(orm.Fn[E, Geometry](fn, args...))
	return out
}

// combine builds a geometry-valued function over two operands, refusing first
// when they are in different coordinate systems.
//
// The result is nullable if either operand is, because these are strict too.
func (g GeomExpr[E]) combine(fn string, other GeomExpr[E]) GeomExpr[E] {
	out := g
	out.kind = AnyKind
	out.nullable = g.nullable || other.nullable
	a, b, ok := binaryArgs(g, other, fn)
	if !ok {
		out.arg = a
		return out
	}
	out.arg = orm.ArgOf(orm.Fn[E, Geometry](fn, a, b))
	return out
}

// Selecting a constructed geometry.
//
// A [GeomExpr] is a way of building SQL; reading its result needs a typed
// handle, and these are it. The two forms exist for the same reason the two
// measurement forms do: the destination has to be able to hold a NULL exactly
// when the expression can produce one.

// Value reads the expression's result as a geometry.
func (g GeomExpr[E]) Value() orm.Value[E, Geometry] {
	if g.nullable {
		return orm.Fn[E, Geometry]("", orm.ArgFail(nullMismatch(
			"reading a geometry expression", "ValueNull")))
	}
	return orm.ValueOf[E, Geometry](g.arg)
}

// ValueNull reads the expression's result as a geometry that may be NULL.
func (g GeomExpr[E]) ValueNull() orm.Value[E, *Geometry] {
	if !g.nullable {
		return orm.FnNull[E, Geometry]("", orm.ArgFail(notNullMismatch(
			"reading a geometry expression", "Value")))
	}
	return orm.ValueOfNull[E, Geometry](g.arg)
}

// Value reads the expression's result as a geography.
func (g GeogExpr[E]) Value() orm.Value[E, Geography] {
	if g.e.nullable {
		return orm.Fn[E, Geography]("", orm.ArgFail(nullMismatch(
			"reading a geography expression", "ValueNull")))
	}
	return orm.ValueOf[E, Geography](g.e.arg)
}

// ValueNull reads the expression's result as a geography that may be NULL.
func (g GeogExpr[E]) ValueNull() orm.Value[E, *Geography] {
	if !g.e.nullable {
		return orm.FnNull[E, Geography]("", orm.ArgFail(notNullMismatch(
			"reading a geography expression", "Value")))
	}
	return orm.ValueOfNull[E, Geography](g.e.arg)
}
