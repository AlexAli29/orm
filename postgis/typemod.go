package postgis

import (
	"fmt"
	"strconv"
	"strings"
)

// The column's declared type.
//
// PostGIS puts three facts in a type modifier — the shape, the dimensionality
// and the coordinate system — and PostgreSQL renders them back through
// format_type exactly as they were declared:
//
//	geometry(PointZ,4326)
//	geography(Point,4326)
//	geometry
//
// That string is the authoritative statement of what a column holds, and it is
// what the generator reads to decide which descriptor to emit and what
// reconciliation compares a Go field against. Parsing it here rather than in
// four places is what keeps those four agreeing.
//
// A bare geometry has no modifier and constrains nothing: it accepts any shape,
// any dimensionality and any SRID, and every row in it may differ. That is a
// real declaration rather than a missing one, so it parses to a [TypeMod] whose
// fields are all zero and whose Constrained method says so.

// Family is which of the two PostgreSQL spatial types a column has.
type Family uint8

// The spatial storage families.
const (
	// NotSpatial is the zero value: this is not a spatial type at all.
	NotSpatial Family = iota
	// FamilyGeometry is PostGIS's geometry: Cartesian, in the SRID's units.
	FamilyGeometry
	// FamilyGeography is PostGIS's geography: on the spheroid, in metres.
	FamilyGeography
)

// String renders the family as PostgreSQL names the type.
func (f Family) String() string {
	switch f {
	case FamilyGeometry:
		return "geometry"
	case FamilyGeography:
		return "geography"
	default:
		return ""
	}
}

// TypeMod is a spatial column's declared type, taken apart.
//
// Zero values mean "the declaration does not constrain this", which is what a
// bare geometry says about all three. They do not mean a default: a column with
// no SRID constraint holds geometries whose SRIDs are their own business, and
// nothing here fills in 4326.
type TypeMod struct {
	// Family is whether the column is geometry or geography, which is the one
	// part of a modifier that is never optional: the two have different
	// operators and different answers for the same question.
	Family Family
	// Kind is the shape the column accepts, or zero for any.
	Kind Kind
	// Dim is the dimensionality the column accepts. It is [XY] both when the
	// column requires two dimensions and when it constrains nothing, which
	// HasDim separates.
	Dim Dim
	// SRID is the coordinate system the column requires, or [UnknownSRID] for
	// none.
	SRID int32
	// hasMod records that the declaration carried a modifier at all, which is
	// what tells geometry(Point) — constrained to XY points in any SRID — from
	// a bare geometry.
	hasMod bool
}

// Spatial reports whether this is a spatial type.
func (m TypeMod) Spatial() bool { return m.Family != NotSpatial }

// Constrained reports whether the declaration constrains the shape,
// dimensionality or coordinate system at all.
//
// It is one answer rather than three because a PostGIS modifier constrains all
// three or none: geometry(Point,4326) requires two dimensions as much as it
// requires a point, and a bare geometry requires nothing.
func (m TypeMod) Constrained() bool { return m.hasMod }

// String renders the type as PostgreSQL writes it, which is what makes a
// desired schema and an introspected one comparable as text.
func (m TypeMod) String() string {
	if m.Family == NotSpatial {
		return ""
	}
	if !m.hasMod {
		return m.Family.String()
	}
	shape := "Geometry"
	if m.Kind != 0 {
		shape = m.Kind.String()
	}
	shape += m.Dim.String()
	if m.SRID == UnknownSRID {
		return fmt.Sprintf("%s(%s)", m.Family, shape)
	}
	return fmt.Sprintf("%s(%s,%d)", m.Family, shape, m.SRID)
}

// ParseTypeMod reads a PostgreSQL spatial type declaration.
//
// It accepts what PostgreSQL renders and what a person writes, which differ
// only in case and spacing: geometry(Point,4326), GEOMETRY(POINTZ, 4326) and
// geometry are all the same declarations they look like. Anything that is not a
// spatial type parses to the zero [TypeMod] with no error, so a caller can ask
// about every column without deciding first.
//
// It is strict about the parts it does understand. A shape PostGIS does not
// have, an SRID that is not a number, a modifier with three parts — each is an
// error naming what was wrong, because a declaration nobody can read is a
// column nobody can generate for.
func ParseTypeMod(declared string) (TypeMod, error) {
	s := strings.TrimSpace(declared)
	base, rest, hasMod := strings.Cut(s, "(")

	var m TypeMod
	switch strings.ToLower(strings.TrimSpace(base)) {
	case "geometry":
		m.Family = FamilyGeometry
	case "geography":
		m.Family = FamilyGeography
	default:
		return TypeMod{}, nil
	}
	if !hasMod {
		return m, nil
	}
	m.hasMod = true

	body, closed := strings.CutSuffix(strings.TrimSpace(rest), ")")
	if !closed {
		return TypeMod{}, fmt.Errorf("postgis: %q has an unclosed type modifier", declared)
	}
	parts := strings.Split(body, ",")
	if len(parts) > 2 {
		return TypeMod{}, fmt.Errorf("postgis: %q has %d parts in its type modifier;"+
			" a spatial modifier is a shape, optionally followed by an SRID", declared, len(parts))
	}

	shape := strings.TrimSpace(parts[0])
	kind, dim, err := parseShape(shape)
	if err != nil {
		return TypeMod{}, fmt.Errorf("postgis: %q: %w", declared, err)
	}
	m.Kind, m.Dim = kind, dim

	if len(parts) == 2 {
		srid := strings.TrimSpace(parts[1])
		n, err := strconv.ParseInt(srid, 10, 32)
		if err != nil {
			return TypeMod{}, fmt.Errorf("postgis: %q: %q is not an SRID;"+
				" an SRID is a number, and it is never a name", declared, srid)
		}
		m.SRID = int32(n)
	}
	return m, nil
}

// parseShape reads a shape name with its optional dimensionality suffix:
// Point, PointZ, MultiPolygonZM, Geometry.
func parseShape(shape string) (Kind, Dim, error) {
	name := strings.ToLower(shape)
	// The suffix is checked longest first, so PointZM is not read as PointZ
	// with a stray M.
	dim := XY
	switch {
	case strings.HasSuffix(name, "zm"):
		dim, name = XYZM, strings.TrimSuffix(name, "zm")
	case strings.HasSuffix(name, "z"):
		dim, name = XYZ, strings.TrimSuffix(name, "z")
	case strings.HasSuffix(name, "m"):
		// A trailing m is a measure suffix unless the name ends in one on its
		// own, which no PostGIS shape name does — the multi forms all end in
		// Point, LineString or Polygon.
		dim, name = XYM, strings.TrimSuffix(name, "m")
	}
	if name == "geometry" {
		// geometry(Geometry,4326) constrains the SRID and the dimensionality
		// and leaves the shape open, which is a form PostGIS accepts.
		return 0, dim, nil
	}
	kind, ok := kindByName[name]
	if !ok {
		return 0, XY, fmt.Errorf("%q is not a shape PostGIS has", shape)
	}
	return kind, dim, nil
}

// GoType names the Go type a column of this declaration reads into.
//
// It is the generator's answer to "what does this field have to be", and it is
// two answers rather than seven: the shape lives in the value and in the
// descriptor's metadata, not in the Go type. A column typed
// geometry(Polygon,4326) reads into a [Geometry] that happens to be a polygon,
// because that is what PostGIS hands back and because ST_Intersection of two
// polygons is not always one.
func (m TypeMod) GoType() string {
	switch m.Family {
	case FamilyGeometry:
		return "github.com/AlexAli29/orm/postgis.Geometry"
	case FamilyGeography:
		return "github.com/AlexAli29/orm/postgis.Geography"
	default:
		return ""
	}
}

// Accepts reports whether a value can be stored in a column of this
// declaration, and says why not when it cannot.
//
// This is the check PostGIS performs on INSERT, done early enough to name the
// field. It is deliberately the same rule rather than a stricter one: a column
// with no modifier accepts anything, and a constrained one accepts exactly what
// its modifier says.
func (m TypeMod) Accepts(g Geometry) error {
	if !m.hasMod {
		return nil
	}
	if m.Kind != 0 && g.Kind() != m.Kind {
		return fmt.Errorf("postgis: this column holds %s and the value is a %s", m, g.Kind())
	}
	if g.Dim() != m.Dim {
		return fmt.Errorf("postgis: this column holds %s and the value is %s", m, dimName(g.Dim()))
	}
	if m.SRID != UnknownSRID && g.SRID() != m.SRID {
		return fmt.Errorf("postgis: this column holds %s and the value is in SRID %d", m, g.SRID())
	}
	return nil
}

// KindConst names the exported Kind constant, for a generator writing Go.
func KindConst(k Kind) string {
	switch k {
	case KindPoint:
		return "KindPoint"
	case KindLineString:
		return "KindLineString"
	case KindPolygon:
		return "KindPolygon"
	case KindMultiPoint:
		return "KindMultiPoint"
	case KindMultiLineString:
		return "KindMultiLineString"
	case KindMultiPolygon:
		return "KindMultiPolygon"
	case KindCollection:
		return "KindCollection"
	default:
		return "AnyKind"
	}
}

// AnyKind is the zero [Kind]: the column constrains no shape.
//
// It is named so that generated code says what it means — a descriptor built
// with AnyKind reads better than one built with 0, and the two are the same
// value.
const AnyKind Kind = 0

// DimConst names the exported Dim constant, for a generator writing Go.
func DimConst(d Dim) string {
	switch d {
	case XYZ:
		return "XYZ"
	case XYM:
		return "XYM"
	case XYZM:
		return "XYZM"
	default:
		return "XY"
	}
}
