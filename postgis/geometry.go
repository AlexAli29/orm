// Package postgis makes PostGIS type-safe without hiding it.
//
// It is opt-in. A project that does not import it never sees a spatial API, and
// the root ORM knows nothing about geometry — everything here composes through
// the one extension boundary that package exposes, so there is no second query
// compiler and no second expression model.
//
// Two distinctions run through the whole package and are never blurred:
//
//	geometry   Cartesian, in whatever units the SRID's coordinate system uses
//	geography  on the spheroid, with distances and lengths in metres
//
// They are different PostgreSQL types with different index behaviour and
// different answers, so they are different Go types here. Converting between
// them is something you write, not something that happens to you.
//
//	the shape    Point, LineString, Polygon, and the multi forms
//	the SRID     which coordinate system the numbers are in
//
// Both travel with the value and with the column, because losing either is how
// a query comes to compare metres with degrees and get a number back.
package postgis

import (
	"fmt"
	"math"
	"slices"
)

// Dim is a geometry's coordinate dimensionality.
//
// PostGIS stores four: the plain XY, XY with a Z ordinate, XY with a measure,
// and both. They are part of the type — geometry(PointZ,4326) is not
// geometry(Point,4326) — so nothing here drops an ordinate to make a value fit.
type Dim uint8

// The dimensionalities PostGIS stores.
const (
	// XY is two ordinates, which is what an unqualified geometry has.
	XY Dim = iota
	// XYZ adds a Z ordinate.
	XYZ
	// XYM adds a measure.
	XYM
	// XYZM has both.
	XYZM
)

// String renders the dimensionality as PostGIS spells it in a type name: the
// empty string for XY, and Z, M or ZM as a suffix for the others.
func (d Dim) String() string {
	switch d {
	case XYZ:
		return "Z"
	case XYM:
		return "M"
	case XYZM:
		return "ZM"
	default:
		return ""
	}
}

// HasZ reports whether the dimensionality carries a Z ordinate.
func (d Dim) HasZ() bool { return d == XYZ || d == XYZM }

// HasM reports whether the dimensionality carries a measure.
func (d Dim) HasM() bool { return d == XYM || d == XYZM }

// ordinates is how many float64s one coordinate occupies.
func (d Dim) ordinates() int {
	switch d {
	case XYZ, XYM:
		return 3
	case XYZM:
		return 4
	default:
		return 2
	}
}

// Kind is a geometry's shape.
type Kind uint8

// The shapes this package models. They are PostGIS's own type codes in the same
// order, which is what makes the codec a lookup rather than a switch.
const (
	KindPoint Kind = iota + 1
	KindLineString
	KindPolygon
	KindMultiPoint
	KindMultiLineString
	KindMultiPolygon
	KindCollection
)

// String renders the shape as PostGIS spells it in a type modifier.
func (k Kind) String() string {
	switch k {
	case KindPoint:
		return "Point"
	case KindLineString:
		return "LineString"
	case KindPolygon:
		return "Polygon"
	case KindMultiPoint:
		return "MultiPoint"
	case KindMultiLineString:
		return "MultiLineString"
	case KindMultiPolygon:
		return "MultiPolygon"
	case KindCollection:
		return "GeometryCollection"
	default:
		return "Geometry"
	}
}

// kindByName maps a PostGIS type-modifier spelling back to a shape, folded to
// lower case because the catalog and a declaration may differ in it.
var kindByName = map[string]Kind{
	"point":              KindPoint,
	"linestring":         KindLineString,
	"polygon":            KindPolygon,
	"multipoint":         KindMultiPoint,
	"multilinestring":    KindMultiLineString,
	"multipolygon":       KindMultiPolygon,
	"geometrycollection": KindCollection,
}

// UnknownSRID is PostGIS's "no spatial reference": SRID 0.
//
// It is not 4326. A geometry that never had an SRID assigned is in an
// unspecified coordinate system, and treating it as longitude and latitude
// because that is the common case is how coordinates end up in the wrong place
// on a map. Nothing here infers one.
const UnknownSRID int32 = 0

// Coord is one position.
//
// Z and M are meaningful only when the geometry's dimensionality says so, which
// is why they are read through the geometry rather than tested for zero: 0 is a
// legitimate elevation.
type Coord struct {
	// X and Y are the horizontal ordinates — longitude and latitude in a
	// geographic reference system, easting and northing in a projected one.
	// Which they are is decided by the SRID, not by this type.
	//
	// Z is elevation and M is a measure: a per-vertex value, most often a
	// distance along a route or a timestamp. Both are zero when the geometry
	// does not carry them, which is why the dimension is recorded separately
	// rather than inferred from these being zero.
	X, Y, Z, M float64
}

// Geometry is any shape, with its dimensionality and spatial reference.
//
// It is one Go type rather than seven because a shape is data rather than a
// type in PostGIS too: a geometry column declared without a modifier holds any
// of them, ST_Intersection returns whichever the answer is, and a Go type per
// shape would make those unrepresentable. What is typed here is the column and
// the expression; the value carries its own shape and reports it.
//
// The coordinate storage is flat and the structure is carried beside it, which
// is what keeps a polygon with a thousand vertices one allocation rather than a
// thousand.
type Geometry struct {
	kind Kind
	dim  Dim
	srid int32
	// coords holds every ordinate of every position, in order.
	coords []float64
	// parts describes the nesting. A point has none, a line has one run, a
	// polygon has one run per ring, and a multi geometry nests once more.
	parts []part
	// sub holds the members of a collection, which are geometries in their own
	// right and may each be a different shape.
	sub []Geometry
	// empty distinguishes an empty geometry from one nobody filled in. It is a
	// real PostGIS value — POLYGON EMPTY is a polygon — and it is not NULL.
	empty bool
}

// part is one contiguous run of positions within the flat coordinate slice.
type part struct {
	// group is which member of a multi geometry the run belongs to.
	group int
	start int
	end   int
}

// Kind reports the geometry's shape.
func (g Geometry) Kind() Kind { return g.kind }

// Dim reports the geometry's dimensionality.
func (g Geometry) Dim() Dim { return g.dim }

// SRID reports the spatial reference the coordinates are in, which is
// [UnknownSRID] when the geometry has none.
func (g Geometry) SRID() int32 { return g.srid }

// IsEmpty reports whether the geometry is one of PostGIS's empty values.
//
// An empty geometry is not NULL. POINT EMPTY is a point that happens to have no
// position, and a column holding one holds a value; a NULL column holds none.
// The two are different in SQL and are different here.
func (g Geometry) IsEmpty() bool { return g.empty || (len(g.coords) == 0 && len(g.sub) == 0) }

// NumPoints reports how many positions the geometry holds, across every ring
// and every member.
func (g Geometry) NumPoints() int {
	n := len(g.coords) / max(g.dim.ordinates(), 1)
	for _, s := range g.sub {
		n += s.NumPoints()
	}
	return n
}

// Coords returns every position of the geometry, in order.
//
// The slice is freshly built, so a caller may keep or modify it without
// reaching back into the geometry — which matters because a geometry inside a
// built query has to stay the geometry that query was built from.
func (g Geometry) Coords() []Coord {
	n := g.dim.ordinates()
	out := make([]Coord, 0, len(g.coords)/max(n, 1))
	for i := 0; i+n <= len(g.coords); i += n {
		c := Coord{X: g.coords[i], Y: g.coords[i+1]}
		switch g.dim {
		case XYZ:
			c.Z = g.coords[i+2]
		case XYM:
			c.M = g.coords[i+2]
		case XYZM:
			c.Z, c.M = g.coords[i+2], g.coords[i+3]
		}
		out = append(out, c)
	}
	return out
}

// Geometries returns the members of a collection or multi geometry.
//
// The result is a copy for the same reason [Geometry.Coords] is.
func (g Geometry) Geometries() []Geometry {
	if len(g.sub) > 0 {
		return slices.Clone(g.sub)
	}
	if g.kind < KindMultiPoint {
		return nil
	}
	// A multi geometry's members are runs of the flat storage rather than
	// separate values, so they are rebuilt on the way out.
	groups := 0
	for _, p := range g.parts {
		groups = max(groups, p.group+1)
	}
	out := make([]Geometry, 0, groups)
	member := memberKind(g.kind)
	for gi := range groups {
		m := Geometry{kind: member, dim: g.dim, srid: g.srid}
		for _, p := range g.parts {
			if p.group != gi {
				continue
			}
			m.parts = append(m.parts, part{start: len(m.coords), end: len(m.coords) + (p.end - p.start)})
			m.coords = append(m.coords, g.coords[p.start:p.end]...)
		}
		if len(m.coords) == 0 {
			// An empty member is empty rather than a shape with an empty ring:
			// POLYGON EMPTY has no rings, and encoding it as one ring of no
			// points is a different value.
			m.empty, m.parts = true, nil
		}
		out = append(out, m)
	}
	return out
}

func memberKind(k Kind) Kind {
	switch k {
	case KindMultiPoint:
		return KindPoint
	case KindMultiLineString:
		return KindLineString
	case KindMultiPolygon:
		return KindPolygon
	default:
		return k
	}
}

// clone returns a geometry sharing nothing with the receiver.
//
// Every constructor calls it. A geometry built from a caller's slice and then
// held by a compiled query has to keep the coordinates it was built from, and
// the M12 audit found the same bug in multiranges: an expression that kept the
// caller's backing array changed meaning when the caller reused it.
func (g Geometry) clone() Geometry {
	out := g
	out.coords = slices.Clone(g.coords)
	out.parts = slices.Clone(g.parts)
	if g.sub != nil {
		out.sub = make([]Geometry, 0, len(g.sub))
		for _, s := range g.sub {
			out.sub = append(out.sub, s.clone())
		}
	}
	return out
}

// WithSRID returns the geometry labelled with a spatial reference.
//
// This is ST_SetSRID's semantics in Go: it changes which coordinate system the
// numbers are said to be in and does not touch the numbers. Use it when the
// coordinates are already right and only the label is missing.
func (g Geometry) WithSRID(srid int32) Geometry {
	out := g.clone()
	out.srid = srid
	return out
}

// Constructors.
//
// Each takes the coordinates and the spatial reference, because a geometry with
// no SRID is a geometry in an unspecified coordinate system and this package
// will not choose one. Pass [UnknownSRID] deliberately when that is what is
// meant.

// NewPoint returns a two-dimensional point.
func NewPoint(srid int32, x, y float64) Geometry {
	return Geometry{kind: KindPoint, dim: XY, srid: srid, coords: []float64{x, y}}
}

// NewPointZ returns a point with an elevation.
func NewPointZ(srid int32, x, y, z float64) Geometry {
	return Geometry{kind: KindPoint, dim: XYZ, srid: srid, coords: []float64{x, y, z}}
}

// NewPointM returns a point with a measure.
func NewPointM(srid int32, x, y, m float64) Geometry {
	return Geometry{kind: KindPoint, dim: XYM, srid: srid, coords: []float64{x, y, m}}
}

// NewPointZM returns a point with both an elevation and a measure.
func NewPointZM(srid int32, x, y, z, m float64) Geometry {
	return Geometry{kind: KindPoint, dim: XYZM, srid: srid, coords: []float64{x, y, z, m}}
}

// EmptyPoint returns POINT EMPTY, which is a point with no position.
func EmptyPoint(srid int32) Geometry {
	return Geometry{kind: KindPoint, dim: XY, srid: srid, empty: true}
}

// NewLineString returns a line through the given positions.
func NewLineString(srid int32, dim Dim, cs ...Coord) Geometry {
	return runGeometry(KindLineString, srid, dim, cs)
}

// NewPolygon returns a polygon from its rings, the first being the exterior one.
//
// PostGIS decides whether the rings are closed and correctly nested; nothing
// here closes a ring for you, because a ring that needed closing is a ring
// somebody got wrong and silently fixing it hides that.
func NewPolygon(srid int32, dim Dim, rings ...[]Coord) Geometry {
	return nestedGeometry(KindPolygon, srid, dim, [][][]Coord{rings})
}

// NewMultiPoint returns several points as one geometry.
func NewMultiPoint(srid int32, dim Dim, cs ...Coord) Geometry {
	g := Geometry{kind: KindMultiPoint, dim: dim, srid: srid}
	for i, c := range cs {
		g.parts = append(g.parts, part{group: i, start: len(g.coords), end: len(g.coords) + dim.ordinates()})
		g.coords = appendCoord(g.coords, dim, c)
	}
	g.empty = len(cs) == 0
	return g
}

// NewMultiLineString returns several lines as one geometry.
func NewMultiLineString(srid int32, dim Dim, lines ...[]Coord) Geometry {
	parts := make([][][]Coord, 0, len(lines))
	for _, l := range lines {
		parts = append(parts, [][]Coord{l})
	}
	return nestedGeometry(KindMultiLineString, srid, dim, parts)
}

// NewMultiPolygon returns several polygons as one geometry, each given as its
// list of rings.
func NewMultiPolygon(srid int32, dim Dim, polygons ...[][]Coord) Geometry {
	return nestedGeometry(KindMultiPolygon, srid, dim, polygons)
}

// NewCollection returns a geometry collection.
//
// The members keep their own shapes. Their spatial references have to agree
// with the collection's, because a collection whose members disagree is not a
// value PostGIS can store.
func NewCollection(srid int32, members ...Geometry) (Geometry, error) {
	g := Geometry{kind: KindCollection, srid: srid, empty: len(members) == 0}
	for i, m := range members {
		if m.srid != srid && m.srid != UnknownSRID {
			return Geometry{}, fmt.Errorf("postgis: collection member %d has SRID %d, and the collection has %d",
				i, m.srid, srid)
		}
		c := m.clone()
		c.srid = srid
		if i == 0 {
			g.dim = c.dim
		} else if c.dim != g.dim {
			return Geometry{}, fmt.Errorf("postgis: collection member %d is %s and the collection is %s;"+
				" PostGIS stores one dimensionality per value", i, dimName(c.dim), dimName(g.dim))
		}
		g.sub = append(g.sub, c)
	}
	return g, nil
}

func dimName(d Dim) string {
	if s := d.String(); s != "" {
		return "XY" + s
	}
	return "XY"
}

func runGeometry(kind Kind, srid int32, dim Dim, cs []Coord) Geometry {
	g := Geometry{kind: kind, dim: dim, srid: srid, empty: len(cs) == 0}
	for _, c := range cs {
		g.coords = appendCoord(g.coords, dim, c)
	}
	if len(cs) > 0 {
		g.parts = []part{{start: 0, end: len(g.coords)}}
	}
	return g
}

// nestedGeometry builds a geometry whose positions come in groups of runs: a
// polygon's rings, a multi-line's lines, a multi-polygon's polygons and rings.
func nestedGeometry(kind Kind, srid int32, dim Dim, groups [][][]Coord) Geometry {
	g := Geometry{kind: kind, dim: dim, srid: srid}
	for gi, runs := range groups {
		for _, run := range runs {
			start := len(g.coords)
			for _, c := range run {
				g.coords = appendCoord(g.coords, dim, c)
			}
			g.parts = append(g.parts, part{group: gi, start: start, end: len(g.coords)})
		}
	}
	g.empty = len(g.coords) == 0
	return g
}

func appendCoord(dst []float64, dim Dim, c Coord) []float64 {
	dst = append(dst, c.X, c.Y)
	switch dim {
	case XYZ:
		dst = append(dst, c.Z)
	case XYM:
		dst = append(dst, c.M)
	case XYZM:
		dst = append(dst, c.Z, c.M)
	}
	return dst
}

// Equal reports whether two geometries hold the same value.
//
// This is structural equality — same shape, same dimensionality, same SRID,
// same coordinates in the same order — and it is deliberately not ST_Equals.
// PostGIS's ST_Equals is a topological question the server answers, and two
// geometries that trace the same shape with different vertex order are equal to
// it and not to this. Ask the server when the topological answer is what is
// meant.
func (g Geometry) Equal(other Geometry) bool {
	if g.kind != other.kind || g.dim != other.dim || g.srid != other.srid ||
		g.IsEmpty() != other.IsEmpty() || len(g.sub) != len(other.sub) {
		return false
	}
	if !slices.Equal(g.parts, other.parts) || len(g.coords) != len(other.coords) {
		return false
	}
	for i := range g.coords {
		// NaN is a legitimate ordinate in PostGIS and is equal to itself here,
		// because two geometries holding the same bits are the same value.
		a, b := g.coords[i], other.coords[i]
		if a != b && !(math.IsNaN(a) && math.IsNaN(b)) {
			return false
		}
	}
	for i := range g.sub {
		if !g.sub[i].Equal(other.sub[i]) {
			return false
		}
	}
	return true
}

// String renders the geometry for a person, in the shape of PostGIS's own EWKT.
//
// It is a diagnostic rather than a serialisation: use ST_AsEWKT when the text
// has to be PostGIS's.
func (g Geometry) String() string {
	s := g.kind.String() + g.dim.String()
	if g.srid != UnknownSRID {
		s = fmt.Sprintf("SRID=%d;%s", g.srid, s)
	}
	if g.IsEmpty() {
		return s + " EMPTY"
	}
	return fmt.Sprintf("%s[%d points]", s, g.NumPoints())
}
