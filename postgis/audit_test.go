package postgis_test

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/postgis"
)

// The final adversarial audit.
//
// Everything here is an attempt to make this package say something false: to
// lose a coordinate, forget an SRID, drop a Z, confuse NULL with EMPTY, or
// promise a shape PostGIS does not guarantee. A test that passes here is a
// claim somebody tried to break.

// A collection whose members disagree about dimensionality is not a value
// PostGIS stores: it answers "Dimensions mismatch in lwcollection" and refuses.
// The constructor already refuses it too. The attack is the decoder, because
// bytes can say anything, and a decoder that normalised the collection to its
// first member would produce a Go value that re-encodes into something the
// server rejects.
func TestAudit_mixedDimensionCollection(t *testing.T) {
	conn := gisConn(t)

	cases := []struct {
		name string
		raw  []byte
	}{
		{
			// Declared XY, containing a POINTZ.
			"header disagrees with its member",
			append([]byte{1, 7, 0, 0, 0, 1, 0, 0, 0, 1, 1, 0, 0, 0x80}, make([]byte, 24)...),
		},
		{
			// Two members that disagree with each other.
			"members disagree with one another",
			func() []byte {
				b := []byte{1, 7, 0, 0, 0, 2, 0, 0, 0}
				b = append(b, 1, 1, 0, 0, 0)
				b = append(b, make([]byte, 16)...)
				b = append(b, 1, 1, 0, 0, 0x80)
				return append(b, make([]byte, 24)...)
			}(),
		},
		{
			// An M member inside a Z collection, which is the pair that would
			// otherwise read one ordinate as the other.
			"Z collection with an M member",
			append([]byte{1, 7, 0, 0, 0x80, 1, 0, 0, 0, 1, 1, 0, 0, 0x40}, make([]byte, 24)...),
		},
	}

	// A multi geometry has the same rule and its own branch in the decoder, so
	// it gets its own cases: a check that exists only for collections would
	// leave every MULTIPOINT, MULTILINESTRING and MULTIPOLYGON unguarded.
	for _, code := range []byte{4, 5, 6} {
		body := []byte{1, code, 0, 0, 0, 2, 0, 0, 0}
		switch code {
		case 4:
			body = append(body, 1, 1, 0, 0, 0)
			body = append(body, make([]byte, 16)...)
			body = append(body, 1, 1, 0, 0, 0x80)
			body = append(body, make([]byte, 24)...)
		case 5:
			body = append(body, 1, 2, 0, 0, 0, 1, 0, 0, 0)
			body = append(body, make([]byte, 16)...)
			body = append(body, 1, 2, 0, 0, 0x80, 1, 0, 0, 0)
			body = append(body, make([]byte, 24)...)
		default:
			body = append(body, 1, 3, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0)
			body = append(body, make([]byte, 16)...)
			body = append(body, 1, 3, 0, 0, 0x80, 1, 0, 0, 0, 1, 0, 0, 0)
			body = append(body, make([]byte, 24)...)
		}
		cases = append(cases, struct {
			name string
			raw  []byte
		}{"a multi geometry of type " + string(rune('0'+code)) + " with members of two dimensionalities", body})
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// PostGIS is the authority: confirm it refuses these bytes.
			var ewkt string
			serverErr := conn.QueryRow(t.Context(),
				`SELECT ST_AsEWKT($1::bytea::geometry)`, c.raw).Scan(&ewkt)
			if serverErr == nil {
				t.Skipf("PostGIS accepts these bytes (%s); the decoder may too", ewkt)
			}

			if _, err := postgis.DecodeEWKB(c.raw); err == nil {
				t.Fatalf("the decoder accepted bytes PostGIS refuses: %v", serverErr)
			}
		})
	}
}

// The child-SRID attack, several levels deep.
//
// A previous audit found the decoder accepting a member whose SRID differed
// from its parent's, which produces a value that cannot be re-encoded. The fix
// has to hold at every level of nesting, not only the first.
func TestAudit_nestedChildSRID(t *testing.T) {
	// A collection containing a collection containing a point, where the
	// innermost point declares its own SRID.
	inner := []byte{1, 1, 0, 0, 0x20} // Point with the SRID flag
	inner = append(inner, 0x10, 0x0e, 0, 0)
	inner = append(inner, make([]byte, 16)...)

	middle := []byte{1, 7, 0, 0, 0, 1, 0, 0, 0}
	middle = append(middle, inner...)

	outer := []byte{1, 7, 0, 0, 0x20}
	outer = append(outer, 0xe6, 0x10, 0, 0) // SRID 4326
	outer = append(outer, 1, 0, 0, 0)
	outer = append(outer, middle...)

	if _, err := postgis.DecodeEWKB(outer); err == nil {
		t.Fatal("a point three levels down declared its own SRID and the decoder accepted it")
	}

	// The same nesting with the SRID agreeing decodes.
	innerOK := []byte{1, 1, 0, 0, 0x20, 0xe6, 0x10, 0, 0}
	innerOK = append(innerOK, make([]byte, 16)...)
	middleOK := append([]byte{1, 7, 0, 0, 0, 1, 0, 0, 0}, innerOK...)
	outerOK := append([]byte{1, 7, 0, 0, 0x20, 0xe6, 0x10, 0, 0, 1, 0, 0, 0}, middleOK...)
	g, err := postgis.DecodeEWKB(outerOK)
	if err != nil {
		t.Fatalf("a nested collection whose SRIDs agree was refused: %v", err)
	}
	if g.SRID() != 4326 {
		t.Errorf("the nested collection decoded as SRID %d", g.SRID())
	}
}

// A member that carries no SRID inherits the enclosing geometry's, which is
// what PostGIS writes.
func TestAudit_memberInheritsSRID(t *testing.T) {
	g, err := postgis.NewCollection(4326,
		postgis.NewPoint(4326, 1, 2),
		postgis.NewLineString(4326, postgis.XY, postgis.Coord{X: 0, Y: 0}, postgis.Coord{X: 1, Y: 1}),
	)
	if err != nil {
		t.Fatal(err)
	}
	back, err := postgis.DecodeEWKB(g.EWKB())
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range back.Geometries() {
		if m.SRID() != 4326 {
			t.Errorf("member %d came back with SRID %d", i, m.SRID())
		}
	}
}

// The bounding-box flag, on every shape it could precede.
//
// PostGIS never sends one, but the format defines the bit. A decoder that
// ignored it would read the box's four doubles as the geometry's first two
// coordinates and return something that looks like a geometry and is not.
func TestAudit_bboxFlagOnEveryShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		code byte
	}{
		{"Point", 1}, {"LineString", 2}, {"Polygon", 3},
		{"MultiPoint", 4}, {"MultiLineString", 5}, {"MultiPolygon", 6},
		{"GeometryCollection", 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte{1, tc.code, 0, 0, 0x10}
			// A plausible bounding box, then a plausible body.
			raw = append(raw, make([]byte, 32)...)
			raw = append(raw, 1, 0, 0, 0)
			raw = append(raw, make([]byte, 16)...)
			_, err := postgis.DecodeEWKB(raw)
			if err == nil {
				t.Fatal("the bounding-box flag was ignored and the box bytes were read as geometry")
			}
			if !strings.Contains(err.Error(), "bounding box") {
				t.Errorf("the error does not name the flag: %v", err)
			}
		})
	}

	// And with the SRID flag set alongside, which is the combination PostGIS
	// would use if it ever wrote one.
	raw := append([]byte{1, 1, 0, 0, 0x30, 0xe6, 0x10, 0, 0}, make([]byte, 48)...)
	if _, err := postgis.DecodeEWKB(raw); err == nil {
		t.Error("the bounding-box flag alongside an SRID was ignored")
	}
}

// Mixed byte order at every nesting level, which EWKB permits and a machine
// that produced a value on another architecture will do.
func TestAudit_mixedByteOrderNested(t *testing.T) {
	// A big-endian MULTIPOINT containing a little-endian POINT and a big-endian
	// POINT, in SRID 4326.
	//
	//   00 20000004 000010E6 00000002
	//     01 01000000 <le doubles>
	//     00 00000001 <be doubles>
	raw := []byte{0x00, 0x20, 0x00, 0x00, 0x04, 0x00, 0x00, 0x10, 0xe6, 0x00, 0x00, 0x00, 0x02}
	// Little-endian (1, 2).
	raw = append(raw, 0x01, 0x01, 0x00, 0x00, 0x00)
	raw = append(raw, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xf0, 0x3f) // 1.0 LE
	raw = append(raw, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x40) // 2.0 LE
	// Big-endian (3, 4).
	raw = append(raw, 0x00, 0x00, 0x00, 0x00, 0x01)
	raw = append(raw, 0x40, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00) // 3.0 BE
	raw = append(raw, 0x40, 0x10, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00) // 4.0 BE

	g, err := postgis.DecodeEWKB(raw)
	if err != nil {
		t.Fatalf("mixed byte order was refused: %v", err)
	}
	want := postgis.NewMultiPoint(4326, postgis.XY,
		postgis.Coord{X: 1, Y: 2}, postgis.Coord{X: 3, Y: 4})
	if !g.Equal(want) {
		t.Errorf("mixed byte order decoded as %s %v, want %v", g, g.Coords(), want.Coords())
	}
}

// Every combination of the four flag bits, so that no impossible state decodes
// into something plausible.
func TestAudit_flagCombinations(t *testing.T) {
	const (
		z    = 0x80
		m    = 0x40
		srid = 0x20
		bbox = 0x10
	)
	for flags := range 16 {
		high := byte(flags << 4)
		raw := []byte{1, 1, 0, 0, high}
		if high&srid != 0 {
			raw = append(raw, 0xe6, 0x10, 0, 0)
		}
		// Enough doubles for the widest dimensionality, so that a decoder that
		// read the wrong number would not simply run out of bytes.
		ordinates := 2
		if high&z != 0 {
			ordinates++
		}
		if high&m != 0 {
			ordinates++
		}
		raw = append(raw, make([]byte, ordinates*8)...)

		g, err := postgis.DecodeEWKB(raw)
		if high&bbox != 0 {
			if err == nil {
				t.Errorf("flags %#x: the bounding-box bit decoded", high)
			}
			continue
		}
		if err != nil {
			t.Errorf("flags %#x: %v", high, err)
			continue
		}
		// The dimensionality has to be exactly what the flags said.
		wantZ, wantM := high&z != 0, high&m != 0
		if g.Dim().HasZ() != wantZ || g.Dim().HasM() != wantM {
			t.Errorf("flags %#x decoded as %v", high, g.Dim())
		}
		if (high&srid != 0) != (g.SRID() != postgis.UnknownSRID) {
			t.Errorf("flags %#x decoded SRID %d", high, g.SRID())
		}
	}
}

// The curved and surface types PostGIS has and this package does not model,
// each rejected by name rather than decoded as the nearest shape it does model.
func TestAudit_curvedTypesRejected(t *testing.T) {
	// 8..15 are CircularString, CompoundCurve, CurvePolygon, MultiCurve,
	// MultiSurface, PolyhedralSurface, TIN and Triangle.
	for code := byte(8); code <= 15; code++ {
		raw := append([]byte{1, code, 0, 0, 0}, make([]byte, 32)...)
		_, err := postgis.DecodeEWKB(raw)
		if err == nil {
			t.Errorf("type code %d decoded as something this package models", code)
			continue
		}
		if !strings.Contains(err.Error(), "not one this package models") {
			t.Errorf("type code %d failed for the wrong reason: %v", code, err)
		}
	}
	// And type 0, which is no geometry at all.
	if _, err := postgis.DecodeEWKB([]byte{1, 0, 0, 0, 0}); err == nil {
		t.Error("type code 0 decoded")
	}
}

// Coordinate extremes, at the edges of what a coordinate system holds.
func TestAudit_coordinateExtremes(t *testing.T) {
	values := []float64{
		0, negZero(), -1, 1e-300, -1e-300, 1e300, -1e300,
		180, -180, 179.999999999, -179.999999999,
		90, -90, 89.999999999, -89.999999999,
		0.1 + 0.2, 1.0 / 3.0,
	}
	for _, v := range values {
		g := postgis.NewPointZM(4326, v, v, v, v)
		back, err := postgis.DecodeEWKB(g.EWKB())
		if err != nil {
			t.Fatalf("%v: %v", v, err)
		}
		cs := back.Coords()
		if len(cs) != 1 {
			t.Fatalf("%v decoded to %d positions", v, len(cs))
		}
		// Bit-exact, because the codec moves the IEEE bits and does not format
		// a number: the encode is math.Float64bits and the decode is
		// math.Float64frombits, with nothing in between that could round.
		if cs[0].X != v || cs[0].Y != v || cs[0].Z != v || cs[0].M != v {
			t.Errorf("%v round-tripped to %+v", v, cs[0])
		}
	}
}

func negZero() float64 {
	var z float64
	return -z
}

// Every shape, at its minimum valid size and at a realistic one, through the
// codec.
func TestAudit_shapeRoundTripSizes(t *testing.T) {
	big := make([]postgis.Coord, 0, 1001)
	for i := range 1000 {
		f := float64(i) / 1000
		big = append(big, postgis.Coord{X: f, Y: f * f})
	}
	big = append(big, big[0])

	cases := []postgis.Geometry{
		postgis.NewPoint(4326, 1, 2),
		// The minimum line is two positions and the minimum ring is four.
		postgis.NewLineString(4326, postgis.XY, postgis.Coord{X: 0, Y: 0}, postgis.Coord{X: 1, Y: 1}),
		postgis.NewLineString(4326, postgis.XY, big...),
		postgis.NewPolygon(4326, postgis.XY, []postgis.Coord{
			{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 0}}),
		postgis.NewPolygon(4326, postgis.XY, big),
		postgis.NewMultiPoint(4326, postgis.XY, postgis.Coord{X: 1, Y: 2}),
		postgis.NewMultiLineString(4326, postgis.XY,
			[]postgis.Coord{{X: 0, Y: 0}, {X: 1, Y: 1}}),
		postgis.NewMultiPolygon(4326, postgis.XY, [][]postgis.Coord{{
			{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 0}}}),
	}
	for i, g := range cases {
		back, err := postgis.DecodeEWKB(g.EWKB())
		if err != nil {
			t.Fatalf("case %d (%s): %v", i, g, err)
		}
		if !back.Equal(g) {
			t.Errorf("case %d (%s) round-tripped to %s", i, g, back)
		}
		if back.NumPoints() != g.NumPoints() {
			t.Errorf("case %d has %d points and came back with %d", i, g.NumPoints(), back.NumPoints())
		}
	}
}

// A nested dimensionality attack: Z and M on shapes other than a point, where a
// stride computed from the wrong dimensionality would misread every ordinate.
func TestAudit_nestedDimensionality(t *testing.T) {
	for _, dim := range []postgis.Dim{postgis.XY, postgis.XYZ, postgis.XYM, postgis.XYZM} {
		// Values chosen so that reading Z as M, or dropping either, is visible.
		cs := []postgis.Coord{
			{X: 1, Y: 2, Z: 300, M: -400},
			{X: 5, Y: 6, Z: 700, M: -800},
			{X: 9, Y: 10, Z: 1100, M: -1200},
			{X: 1, Y: 2, Z: 300, M: -400},
		}
		for _, g := range []postgis.Geometry{
			postgis.NewLineString(4326, dim, cs...),
			postgis.NewPolygon(4326, dim, cs),
			postgis.NewMultiPoint(4326, dim, cs...),
			postgis.NewMultiLineString(4326, dim, cs, cs),
			postgis.NewMultiPolygon(4326, dim, [][]postgis.Coord{cs}, [][]postgis.Coord{cs, cs}),
		} {
			back, err := postgis.DecodeEWKB(g.EWKB())
			if err != nil {
				t.Fatalf("%v %s: %v", dim, g.Kind(), err)
			}
			if back.Dim() != dim {
				t.Errorf("%v %s came back %v", dim, g.Kind(), back.Dim())
			}
			if !back.Equal(g) {
				t.Errorf("%v %s round-tripped to %v, want %v", dim, g.Kind(), back.Coords(), g.Coords())
			}
			// And the ordinates the dimensionality does not carry are not
			// invented.
			for _, c := range back.Coords() {
				if !dim.HasZ() && c.Z != 0 {
					t.Errorf("%v %s invented Z=%v", dim, g.Kind(), c.Z)
				}
				if !dim.HasM() && c.M != 0 {
					t.Errorf("%v %s invented M=%v", dim, g.Kind(), c.M)
				}
			}
		}
	}
}

// A collection of collections, each carrying a dimensionality.
func TestAudit_collectionDimensionality(t *testing.T) {
	for _, dim := range []postgis.Dim{postgis.XY, postgis.XYZ, postgis.XYM, postgis.XYZM} {
		pt := pointFor(dim)
		line := postgis.NewLineString(4326, dim,
			postgis.Coord{X: 0, Y: 0, Z: 1, M: 2}, postgis.Coord{X: 3, Y: 4, Z: 5, M: 6})
		inner, err := postgis.NewCollection(4326, pt, line)
		if err != nil {
			t.Fatalf("%v: %v", dim, err)
		}
		outer, err := postgis.NewCollection(4326, inner, pt)
		if err != nil {
			t.Fatalf("%v: %v", dim, err)
		}
		back, err := postgis.DecodeEWKB(outer.EWKB())
		if err != nil {
			t.Fatalf("%v: %v", dim, err)
		}
		if !back.Equal(outer) {
			t.Errorf("%v: a nested collection round-tripped differently", dim)
		}
		if back.Dim() != dim {
			t.Errorf("%v: the nested collection came back %v", dim, back.Dim())
		}
	}
}

func pointFor(dim postgis.Dim) postgis.Geometry {
	switch dim {
	case postgis.XYZ:
		return postgis.NewPointZ(4326, 1, 2, 3)
	case postgis.XYM:
		return postgis.NewPointM(4326, 1, 2, 4)
	case postgis.XYZM:
		return postgis.NewPointZM(4326, 1, 2, 3, 4)
	default:
		return postgis.NewPoint(4326, 1, 2)
	}
}

// The build-time SRID check, over every operation that requires agreement.
func TestAudit_knownSRIDMismatchEverywhere(t *testing.T) {
	a := postgis.Value[orm.Composed](postgis.NewPolygon(4326, postgis.XY, square(0, 0, 10)))
	b := postgis.Value[orm.Composed](postgis.NewPolygon(3857, postgis.XY, square(0, 0, 10)))

	predicates := map[string]orm.Predicate[orm.Composed]{
		"Intersects":     a.Intersects(b),
		"Disjoint":       a.Disjoint(b),
		"Contains":       a.Contains(b),
		"Within":         a.Within(b),
		"Covers":         a.Covers(b),
		"CoveredBy":      a.CoveredBy(b),
		"Overlaps":       a.Overlaps(b),
		"Touches":        a.Touches(b),
		"Crosses":        a.Crosses(b),
		"EqualsGeom":     a.EqualsGeom(b),
		"DWithin":        a.DWithin(b, 1),
		"DFullyWithin":   a.DFullyWithin(b, 1),
		"BBoxIntersects": a.BBoxIntersects(b),
		"BBoxContains":   a.BBoxContains(b),
		"BBoxWithin":     a.BBoxWithin(b),
		"BBoxSame":       a.BBoxSame(b),
		"Relate":         a.Relate(b, "T********"),
	}
	for name, p := range predicates {
		t.Run(name, func(t *testing.T) {
			_, _, err := orm.Compose(nil, orm.Project1(orm.Of(orm.BoolOf(p)),
				func(x bool) bool { return x })).From(oneRow).SQL()
			if err == nil {
				t.Fatalf("%s built a statement across SRID 4326 and 3857", name)
			}
			if !strings.Contains(err.Error(), "4326") || !strings.Contains(err.Error(), "3857") {
				t.Errorf("the error does not name both: %v", err)
			}
		})
	}

	values := map[string]func() (string, []any, error){
		"Distance":     scalarSQL(a.Distance(b)),
		"MaxDistance":  scalarSQL(a.MaxDistance(b)),
		"KNNDistance":  scalarSQL(a.KNNDistance(b)),
		"BBoxDistance": scalarSQL(a.BBoxDistance(b)),
		"Azimuth":      nullScalarSQL(a.Azimuth(b)),
		"Intersection": exprSQL(a.Intersection(b).Value()),
		"Union":        exprSQL(a.Union(b).Value()),
		"Difference":   exprSQL(a.Difference(b).Value()),
		"SymDiff":      exprSQL(a.SymDifference(b).Value()),
	}
	for name, build := range values {
		t.Run(name, func(t *testing.T) {
			if _, _, err := build(); err == nil {
				t.Fatalf("%s built a statement across SRID 4326 and 3857", name)
			}
		})
	}
}

// An unknown SRID is not compatible with a known one by fiat, and it is not
// 4326. What this package does is decline to check, which is a different thing
// from declaring them equal — and the server still refuses.
func TestAudit_unknownSRIDIsNotInferred(t *testing.T) {
	conn := gisConn(t)
	mustExec(t, conn, oneRowDDL)

	unknown := postgis.Value[orm.Composed](postgis.NewPoint(postgis.UnknownSRID, 1, 2))
	known := postgis.Value[orm.Composed](postgis.NewPoint(4326, 1, 2))

	if unknown.DeclaredSRID() != postgis.UnknownSRID {
		t.Fatalf("an unlabelled geometry reports SRID %d", unknown.DeclaredSRID())
	}

	// It builds, because a geometry with no SRID may legitimately be one this
	// package cannot check.
	sql, args, err := orm.Compose(nil, orm.Project1(
		orm.Of(orm.BoolOf(unknown.Intersects(known))), func(b bool) bool { return b },
	)).From(oneRow).SQL()
	if err != nil {
		t.Fatalf("building: %v", err)
	}
	// And the server refuses it, which is the check this package deferred to.
	if _, err := conn.Exec(t.Context(), sql, args...); err == nil {
		t.Error("PostGIS accepted a geometry with no SRID against one in 4326")
	}
}

// Transform's metadata must be the target, and a chain must not leave a stale
// intermediate behind.
func TestAudit_transformChainMetadata(t *testing.T) {
	g := postgis.Value[orm.Composed](postgis.NewPoint(4326, 1, 2))

	if got := g.Transform(3857).DeclaredSRID(); got != 3857 {
		t.Errorf("one transform reports %d", got)
	}
	if got := g.Transform(3857).Transform(4326).DeclaredSRID(); got != 4326 {
		t.Errorf("a two-step chain reports %d", got)
	}
	if got := g.Transform(3857).Transform(2263).Transform(4326).DeclaredSRID(); got != 4326 {
		t.Errorf("a three-step chain reports %d", got)
	}
	// And the transformed expression relates to one in the target system.
	if _, _, err := orm.Compose(nil, orm.Project1(
		orm.Of(orm.BoolOf(g.Transform(3857).Intersects(
			postgis.Value[orm.Composed](postgis.NewPoint(3857, 0, 0))))),
		func(b bool) bool { return b },
	)).From(oneRow).SQL(); err != nil {
		t.Errorf("a transformed expression does not relate to its target system: %v", err)
	}
	// While the untransformed one does not.
	if _, _, err := orm.Compose(nil, orm.Project1(
		orm.Of(orm.BoolOf(g.Intersects(postgis.Value[orm.Composed](postgis.NewPoint(3857, 0, 0))))),
		func(b bool) bool { return b },
	)).From(oneRow).SQL(); err == nil {
		t.Error("an untransformed 4326 expression related to a 3857 one")
	}
}

// SetSRID keeps the coordinates and Transform moves them. This is the one
// distinction whose confusion silently relocates every row.
func TestAudit_setSRIDversusTransform(t *testing.T) {
	conn := gisConn(t)
	mustExec(t, conn, oneRowDDL)

	pt := postgis.Value[orm.Composed](postgis.NewPoint(4326, 10, 20))

	read := func(g postgis.GeomExpr[orm.Composed]) (int32, float64, float64) {
		t.Helper()
		sql, args, err := orm.Compose(nil, orm.Project1(g.Value(),
			func(x postgis.Geometry) postgis.Geometry { return x })).From(oneRow).SQL()
		if err != nil {
			t.Fatalf("building: %v", err)
		}
		inner := strings.TrimSuffix(sql, ";")
		var srid int32
		var x, y float64
		q := `SELECT ST_SRID(g), ST_X(g), ST_Y(g) FROM (` + inner + `) AS t(g)`
		if err := conn.QueryRow(t.Context(), q, args...).Scan(&srid, &x, &y); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return srid, x, y
	}

	setSRID, sx, sy := read(pt.SetSRID(3857))
	transformed, tx, ty := read(pt.Transform(3857))

	if setSRID != 3857 || transformed != 3857 {
		t.Fatalf("SetSRID gave SRID %d and Transform gave %d", setSRID, transformed)
	}
	// SetSRID relabelled and changed nothing.
	if sx != 10 || sy != 20 {
		t.Errorf("SetSRID moved the coordinates to (%v, %v)", sx, sy)
	}
	// Transform reprojected: ten degrees of longitude is over a million metres.
	if tx < 1_000_000 || tx > 1_200_000 {
		t.Errorf("Transform gave x=%v, which is not metres", tx)
	}
	// And the two are not the same operation, which is the whole point.
	if sx == tx && sy == ty {
		t.Fatal("SetSRID and Transform produced the same coordinates; one of them is the other")
	}
}

// NULL, EMPTY and a normal value, through every output path, for several
// shapes. Collapsing any two of the three is a data-loss bug.
func TestAudit_nullEmptyNormalMatrix(t *testing.T) {
	conn := gisConn(t)
	mustExec(t, conn, `CREATE TABLE three (
		id int primary key, kind text not null, g geometry(Polygon,4326), p geometry(Point,4326))`)
	if _, err := conn.Exec(t.Context(), `
		INSERT INTO three VALUES
		  (1, 'null',   NULL, NULL),
		  (2, 'empty',  'SRID=4326;POLYGON EMPTY', 'SRID=4326;POINT EMPTY'),
		  (3, 'normal', 'SRID=4326;POLYGON((0 0,1 0,1 1,0 1,0 0))', 'SRID=4326;POINT(1 2)')`); err != nil {
		t.Fatal(err)
	}

	type row struct {
		kind    string
		geom    *postgis.Geometry
		isNull  bool
		isEmpty *bool
		area    *float64
		text    *string
		ewkt    *string
		json    *string
		binary  *[]byte
		centre  *postgis.Geometry
	}
	rows, err := conn.Query(t.Context(), `
		SELECT kind, g, g IS NULL, ST_IsEmpty(g), ST_Area(g),
		       ST_AsText(g), ST_AsEWKT(g), ST_AsGeoJSON(g), ST_AsBinary(g), ST_Centroid(g)
		FROM three ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.kind, &r.geom, &r.isNull, &r.isEmpty, &r.area,
			&r.text, &r.ewkt, &r.json, &r.binary, &r.centre); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("read %d rows", len(got))
	}

	// NULL: every derived value is NULL too, because all of these are strict.
	n := got[0]
	if n.geom != nil || !n.isNull {
		t.Errorf("the NULL row read back a geometry: %v", n.geom)
	}
	for name, v := range map[string]bool{
		"IsEmpty": n.isEmpty != nil, "Area": n.area != nil, "AsText": n.text != nil,
		"AsEWKT": n.ewkt != nil, "AsGeoJSON": n.json != nil, "AsBinary": n.binary != nil,
		"Centroid": n.centre != nil,
	} {
		if v {
			t.Errorf("%s of NULL came back with a value", name)
		}
	}

	// EMPTY: a value everywhere, and empty where that is the answer.
	e := got[1]
	if e.geom == nil {
		t.Fatal("POLYGON EMPTY read back as NULL")
	}
	if !e.geom.IsEmpty() {
		t.Errorf("POLYGON EMPTY read back as %s", e.geom)
	}
	if e.geom.Kind() != postgis.KindPolygon {
		t.Errorf("POLYGON EMPTY read back as a %s", e.geom.Kind())
	}
	if e.geom.SRID() != 4326 {
		t.Errorf("POLYGON EMPTY lost its SRID: %d", e.geom.SRID())
	}
	if e.isEmpty == nil || !*e.isEmpty {
		t.Errorf("ST_IsEmpty of POLYGON EMPTY is %v", e.isEmpty)
	}
	if e.area == nil || *e.area != 0 {
		t.Errorf("ST_Area of POLYGON EMPTY is %v", e.area)
	}
	for name, v := range map[string]bool{
		"AsText": e.text == nil, "AsEWKT": e.ewkt == nil,
		"AsGeoJSON": e.json == nil, "AsBinary": e.binary == nil, "Centroid": e.centre == nil,
	} {
		if v {
			t.Errorf("%s of EMPTY came back NULL, which is the NULL/EMPTY collapse", name)
		}
	}
	if e.centre != nil && !e.centre.IsEmpty() {
		t.Errorf("the centroid of an empty polygon is %s", e.centre)
	}

	// Normal.
	v := got[2]
	if v.geom == nil || v.geom.IsEmpty() {
		t.Errorf("the normal row read back %v", v.geom)
	}
	if v.area == nil || *v.area != 1 {
		t.Errorf("the unit square has area %v", v.area)
	}
}

// An empty point, which PostGIS stores as all-NaN ordinates, must not be
// confused with a point at the origin.
func TestAudit_emptyPointIsNotOrigin(t *testing.T) {
	conn := gisConn(t)
	empty := postgis.EmptyPoint(4326)
	origin := postgis.NewPoint(4326, 0, 0)

	if empty.Equal(origin) {
		t.Fatal("POINT EMPTY equals POINT(0 0) in Go")
	}

	var emptyBack, originBack postgis.Geometry
	var emptyIs, originIs bool
	if err := conn.QueryRow(t.Context(),
		`SELECT $1::geometry, ST_IsEmpty($1::geometry), $2::geometry, ST_IsEmpty($2::geometry)`,
		empty, origin).Scan(&emptyBack, &emptyIs, &originBack, &originIs); err != nil {
		t.Fatal(err)
	}
	if !emptyIs || originIs {
		t.Errorf("PostGIS says empty=%v and origin-empty=%v", emptyIs, originIs)
	}
	if !emptyBack.IsEmpty() || originBack.IsEmpty() {
		t.Errorf("they came back as %s and %s", emptyBack, originBack)
	}
	if emptyBack.Equal(originBack) {
		t.Error("they came back equal")
	}
}
