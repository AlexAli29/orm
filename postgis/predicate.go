package postgis

import (
	"fmt"

	"github.com/AlexAli29/orm"
)

// Spatial predicates.
//
// These are ordinary [orm.Predicate] values. They go into Where beside
// Users.Active.Eq(true), compose with And, Or and Not, and are rendered by the
// same writer — there is no spatial query builder, because a spatial query is a
// query.
//
// Three-valued logic is untouched. ST_Intersects of a NULL geometry is NULL,
// which is UNKNOWN, which does not match — the same as every other comparison
// against NULL in SQL. Nothing here converts that to false, because a row whose
// geometry is missing is not a row that fails the test; it is a row the test
// cannot be applied to, and WHERE and CHECK treat those differently.
//
// Two mistakes the server would only catch at run time are caught here instead.
// Relating a geometry to a geography does not compile at all, because they are
// different Go types. Relating geometries in two coordinate systems compiles
// and then refuses to build a statement, with a message naming both SRIDs —
// which is as early as it can be caught, since a column states its coordinate
// system in its type modifier and a Go type cannot carry a number.

// The topological relationships, as PostGIS's OGC predicates.
//
// Each takes the other operand as an expression, which a column becomes with
// Expr and a value becomes with [Value]. The bounding-box index is used
// automatically: every one of these has an && on the box in front of the exact
// test, which is what makes a GiST index apply.

// Intersects reports whether the two geometries share any point at all.
//
// It is the negation of ST_Disjoint and the predicate most spatial queries
// want. An empty geometry intersects nothing, including itself.
func (g GeomExpr[E]) Intersects(other GeomExpr[E]) orm.Predicate[E] {
	return g.relate("ST_Intersects", other)
}

// Disjoint reports whether the two geometries share no point.
//
// PostGIS does not index this one: there is no bounding box that proves two
// geometries do not touch, so it is a full scan. Prefer Not(Intersects) when
// the query has an index to use.
func (g GeomExpr[E]) Disjoint(other GeomExpr[E]) orm.Predicate[E] {
	return g.relate("ST_Disjoint", other)
}

// Contains reports whether the receiver contains the other geometry entirely,
// with no point of the other outside it and at least one point inside.
//
// A geometry does not contain a point that lies exactly on its boundary. That
// is ST_Covers, and the difference is the boundary — which is where the points
// that matter usually are.
func (g GeomExpr[E]) Contains(other GeomExpr[E]) orm.Predicate[E] {
	return g.relate("ST_Contains", other)
}

// ContainsProperly reports whether the other geometry lies in the receiver's
// interior, touching no part of its boundary.
func (g GeomExpr[E]) ContainsProperly(other GeomExpr[E]) orm.Predicate[E] {
	return g.relate("ST_ContainsProperly", other)
}

// Within reports whether the receiver lies entirely inside the other geometry.
// It is Contains with the operands the other way round.
func (g GeomExpr[E]) Within(other GeomExpr[E]) orm.Predicate[E] {
	return g.relate("ST_Within", other)
}

// Covers reports whether no point of the other geometry lies outside the
// receiver.
//
// This is the one that includes the boundary, and it is usually what somebody
// means by "contains": a point on the edge of a district is in the district.
func (g GeomExpr[E]) Covers(other GeomExpr[E]) orm.Predicate[E] {
	return g.relate("ST_Covers", other)
}

// CoveredBy reports whether no point of the receiver lies outside the other
// geometry. It is Covers with the operands the other way round.
func (g GeomExpr[E]) CoveredBy(other GeomExpr[E]) orm.Predicate[E] {
	return g.relate("ST_CoveredBy", other)
}

// Overlaps reports whether the two geometries share some but not all of their
// points, and have the same dimension.
//
// Two polygons that partly cover one another overlap. A polygon and a point do
// not, whatever their positions, because their dimensions differ.
func (g GeomExpr[E]) Overlaps(other GeomExpr[E]) orm.Predicate[E] {
	return g.relate("ST_Overlaps", other)
}

// Touches reports whether the two geometries meet at their boundaries and share
// no interior point.
func (g GeomExpr[E]) Touches(other GeomExpr[E]) orm.Predicate[E] {
	return g.relate("ST_Touches", other)
}

// Crosses reports whether the two geometries pass through one another: a line
// crossing a polygon, or two lines meeting at a point.
func (g GeomExpr[E]) Crosses(other GeomExpr[E]) orm.Predicate[E] {
	return g.relate("ST_Crosses", other)
}

// EqualsGeom reports whether the two geometries occupy the same space.
//
// This is topological equality, which is not the same as being made of the same
// vertices: a line drawn backwards equals itself, and a polygon with a repeated
// point equals the one without. It is the question a database should answer,
// and [Geometry.Equal] in Go deliberately answers the other one.
//
// The name is not Equals because the embedded column descriptor already has Eq,
// which is PostgreSQL's = — an exact comparison of the stored representation,
// and a different question again.
func (g GeomExpr[E]) EqualsGeom(other GeomExpr[E]) orm.Predicate[E] {
	return g.relate("ST_Equals", other)
}

// DWithin reports whether the two geometries are within distance of one
// another, measured in the units of their coordinate system.
//
// The units are the trap. Over geometry(4326) the distance is in degrees, and a
// degree is about 111 km at the equator and much less near the poles — so a
// query written with 1000 for "a kilometre" selects most of a continent. Use a
// geography, or a projected coordinate system, when the answer should be in
// metres; see [GeogExpr.DWithin].
//
// It is the predicate a GiST index accelerates best, because PostGIS expands
// the bounding box by the distance and searches that.
func (g GeomExpr[E]) DWithin(other GeomExpr[E], distance float64) orm.Predicate[E] {
	a, b, ok := binaryArgs(g, other, "ST_DWithin")
	if !ok {
		return orm.FnPredicate[E]("ST_DWithin", a, b)
	}
	return orm.FnPredicate[E]("ST_DWithin", a, b, orm.ArgCast(distance, "float8"))
}

// DFullyWithin reports whether every part of each geometry is within distance
// of every part of the other, which is a question about the farthest points
// rather than the nearest.
func (g GeomExpr[E]) DFullyWithin(other GeomExpr[E], distance float64) orm.Predicate[E] {
	a, b, ok := binaryArgs(g, other, "ST_DFullyWithin")
	if !ok {
		return orm.FnPredicate[E]("ST_DFullyWithin", a, b)
	}
	return orm.FnPredicate[E]("ST_DFullyWithin", a, b, orm.ArgCast(distance, "float8"))
}

// BBoxIntersects builds the && operator: the two bounding boxes overlap.
//
// It is the cheap test the exact predicates run first, exposed on its own
// because sometimes the box is the question — a viewport, a tile, a coarse
// filter before something expensive. It is not a substitute for [Intersects]:
// two geometries whose boxes overlap need not touch at all.
func (g GeomExpr[E]) BBoxIntersects(other GeomExpr[E]) orm.Predicate[E] {
	return g.op("&&", other)
}

// BBoxContains builds the ~ operator: the receiver's bounding box contains the
// other's.
func (g GeomExpr[E]) BBoxContains(other GeomExpr[E]) orm.Predicate[E] {
	return g.op("~", other)
}

// BBoxWithin builds the @ operator: the receiver's bounding box is contained by
// the other's.
func (g GeomExpr[E]) BBoxWithin(other GeomExpr[E]) orm.Predicate[E] {
	return g.op("@", other)
}

// BBoxSame builds the ~= operator: the two bounding boxes are identical.
func (g GeomExpr[E]) BBoxSame(other GeomExpr[E]) orm.Predicate[E] {
	return g.op("~=", other)
}

// Relate builds ST_Relate against a DE-9IM intersection matrix pattern.
//
// The pattern is nine characters describing how the interiors, boundaries and
// exteriors of the two geometries meet — the general form every named predicate
// above is a special case of. It is validated here rather than sent as written,
// because a pattern is a value from the caller and it reaches the server as a
// bind parameter either way; the check is so a typo fails with a message about
// the pattern instead of a PostGIS error about a matrix.
func (g GeomExpr[E]) Relate(other GeomExpr[E], pattern string) orm.Predicate[E] {
	if err := validDE9IM(pattern); err != nil {
		return orm.FnPredicate[E]("ST_Relate", orm.ArgFail(err))
	}
	a, b, ok := binaryArgs(g, other, "ST_Relate")
	if !ok {
		return orm.FnPredicate[E]("ST_Relate", a, b)
	}
	return orm.FnPredicate[E]("ST_Relate", a, b, orm.ArgCast(pattern, "text"))
}

// validDE9IM checks a DE-9IM pattern's shape: nine characters, each a dimension
// digit, a wildcard, or a requirement that the intersection be empty.
func validDE9IM(pattern string) error {
	if len(pattern) != 9 {
		return fmt.Errorf("postgis: a DE-9IM pattern is nine characters and %q is %d",
			pattern, len(pattern))
	}
	for i := range pattern {
		switch pattern[i] {
		case '0', '1', '2', 'T', 'F', '*':
		default:
			return fmt.Errorf("postgis: %q is not a DE-9IM pattern: character %d is %q,"+
				" and the pattern accepts only 0, 1, 2, T, F and *", pattern, i+1, pattern[i])
		}
	}
	return nil
}

// IsValid reports whether the geometry is one PostGIS considers well formed: a
// polygon whose rings close and do not cross themselves, and so on.
//
// An invalid geometry is stored happily and gives wrong answers to almost every
// predicate, so this is worth asking about data that came from somewhere else.
func (g GeomExpr[E]) IsValid() orm.Predicate[E] {
	return orm.FnPredicate[E]("ST_IsValid", g.arg)
}

// IsEmptyGeom reports whether the geometry has no points.
//
// It is not a NULL test. A NULL column has no geometry; an empty geometry is a
// geometry that covers nothing, and the two travel different paths through
// every query. Ask IsNull for the other question — the nullable descriptors
// have it, and the non-nullable ones deliberately do not.
func (g GeomExpr[E]) IsEmptyGeom() orm.Predicate[E] {
	return orm.FnPredicate[E]("ST_IsEmpty", g.arg)
}

// IsSimple reports whether the geometry has no anomalous points: a line that
// does not cross itself, a multi-point with no repeats.
func (g GeomExpr[E]) IsSimple() orm.Predicate[E] {
	return orm.FnPredicate[E]("ST_IsSimple", g.arg)
}

// relate builds one of the two-operand predicates, refusing first when the
// operands cannot legally be related.
// relate builds one of the two-operand predicates, refusing first when the
// operands are in different coordinate systems.
//
// There is no check here that both sides are geometries, because there is no
// way to write a call where they are not: [GeogExpr] is a distinct Go type and
// its operations take geographies. Relating a geometry to a geography does not
// compile, which is a better answer than any error message.
func (g GeomExpr[E]) relate(fn string, other GeomExpr[E]) orm.Predicate[E] {
	a, b, _ := binaryArgs(g, other, fn)
	return orm.FnPredicate[E](fn, a, b)
}

func (g GeomExpr[E]) op(op string, other GeomExpr[E]) orm.Predicate[E] {
	a, b, _ := binaryArgs(g, other, op)
	return orm.OpPredicate[E](op, a, b)
}

// Geography predicates.
//
// PostGIS defines only a few relationships on the spheroid, because most of
// them have no cheap spherical implementation. These are the ones it has, and
// the absence of the rest here is the absence in PostGIS rather than a gap: a
// geography that needs ST_Touches is a geography that should be cast with
// AsGeometry, in a projection the caller chose.

// Intersects reports whether the two geographies share any point.
func (g GeogExpr[E]) Intersects(other GeogExpr[E]) orm.Predicate[E] {
	return g.e.relate("ST_Intersects", other.e)
}

// Covers reports whether no point of the other geography lies outside the
// receiver.
func (g GeogExpr[E]) Covers(other GeogExpr[E]) orm.Predicate[E] {
	return g.e.relate("ST_Covers", other.e)
}

// CoveredBy reports whether no point of the receiver lies outside the other
// geography.
func (g GeogExpr[E]) CoveredBy(other GeogExpr[E]) orm.Predicate[E] {
	return g.e.relate("ST_CoveredBy", other.e)
}

// DWithin reports whether the two geographies are within metres of one another.
//
// Metres, on the spheroid, whatever the latitude — which is the whole reason to
// store a geography. The plane form of this takes the coordinate system's units
// and is a different question; see [GeomExpr.DWithin].
func (g GeogExpr[E]) DWithin(other GeogExpr[E], metres float64) orm.Predicate[E] {
	return g.e.DWithin(other.e, metres)
}

// BBoxIntersects builds the && operator over the geographies' bounding boxes on
// the sphere.
func (g GeogExpr[E]) BBoxIntersects(other GeogExpr[E]) orm.Predicate[E] {
	return g.e.op("&&", other.e)
}
