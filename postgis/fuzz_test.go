package postgis_test

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/postgis"
)

// Fuzzing the spatial surface.
//
// Two things are being looked for. The parsers — EWKB, the type modifier — must
// never panic or allocate from a length they have not read, whatever bytes
// arrive. And the expression builders must produce a statement that is
// deterministic and whose values are all still parameters, whatever composition
// somebody writes.
//
// Neither of these is looking for a wrong answer. A wrong answer is what the
// differential tests are for; these are looking for the failures that are not
// answers at all.

// FuzzDecodeEWKB attacks the decoder with arbitrary bytes.
//
// Every input either decodes to a geometry that re-encodes to itself, or is an
// error. There is no third outcome, and in particular there is no panic and no
// allocation driven by a length the bytes do not back.
func FuzzDecodeEWKB(f *testing.F) {
	for _, g := range []postgis.Geometry{
		postgis.NewPoint(4326, 1, 2),
		postgis.NewPointZM(4326, 1, 2, 3, 4),
		postgis.EmptyPoint(4326),
		postgis.NewLineString(4326, postgis.XY, postgis.Coord{X: 0, Y: 0}, postgis.Coord{X: 1, Y: 1}),
		postgis.NewPolygon(4326, postgis.XY, square(0, 0, 10), square(3, 3, 4)),
		postgis.NewMultiPoint(4326, postgis.XY, postgis.Coord{X: 1, Y: 2}),
		postgis.NewMultiPolygon(4326, postgis.XY, [][]postgis.Coord{square(0, 0, 1)}),
	} {
		f.Add(g.EWKB())
	}
	f.Add([]byte(nil))
	f.Add([]byte{1, 2, 0, 0, 0, 0xff, 0xff, 0xff, 0xff})

	f.Fuzz(func(t *testing.T, raw []byte) {
		g, err := postgis.DecodeEWKB(raw)
		if err != nil {
			return
		}
		// A geometry that decoded has to re-encode to something that decodes to
		// the same value, or the two halves of the codec disagree and a round
		// trip through the database is not one.
		again, err := postgis.DecodeEWKB(g.EWKB())
		if err != nil {
			t.Fatalf("a decoded geometry did not re-decode: %v\ninput %x", err, raw)
		}
		if !again.Equal(g) {
			t.Fatalf("re-encoding changed the value:\n got %s %v\nwant %s %v",
				again, again.Coords(), g, g.Coords())
		}
	})
}

// FuzzParseTypeMod attacks the type-modifier parser, which reads text from a
// struct tag and from format_type.
func FuzzParseTypeMod(f *testing.F) {
	for _, s := range []string{
		"geometry", "geography", "geometry(Point,4326)", "geography(Point,4326)",
		"geometry(MultiPolygonZM,3857)", "geometry(Geometry,0)", "text", "",
		"geometry(", "geometry(Point", "geometry(Point,)", "geometry(,4326)",
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, s string) {
		m, err := postgis.ParseTypeMod(s)
		if err != nil || !m.Spatial() {
			return
		}
		// Anything that parsed has to render to something that parses back to
		// itself, because that rendering is what a migration compares.
		again, err := postgis.ParseTypeMod(m.String())
		if err != nil {
			t.Fatalf("%q parsed to %q, which does not parse: %v", s, m, err)
		}
		if again.String() != m.String() {
			t.Fatalf("%q parsed to %q and re-parsed to %q", s, m, again)
		}
		if again.Family != m.Family || again.Kind != m.Kind ||
			again.Dim != m.Dim || again.SRID != m.SRID {
			t.Fatalf("%q lost a field through a round trip: %+v then %+v", s, m, again)
		}
	})
}

// FuzzGeomFromText asserts that whatever text arrives, the statement is the
// same and the text is an argument.
func FuzzGeomFromText(f *testing.F) {
	f.Add("POINT(1 2)", int32(4326))
	f.Add("'; DROP TABLE places; --", int32(4326))
	f.Add("", int32(0))

	// The baseline is what a benign input produces; nothing else may differ
	// from it.
	baseline, _, err := textStatement("POINT(1 2)", 4326)
	if err != nil {
		f.Fatal(err)
	}

	f.Fuzz(func(t *testing.T, wkt string, srid int32) {
		sql, args, err := textStatement(wkt, srid)
		if err != nil {
			t.Fatalf("building a statement from %q: %v", wkt, err)
		}
		if sql != baseline {
			t.Fatalf("the text changed the statement:\n%s\nwant\n%s", sql, baseline)
		}
		if !bound(args, wkt) {
			t.Fatalf("the text did not reach the argument list: %v", args)
		}
		if !bound(args, srid) {
			t.Fatalf("the SRID did not reach the argument list: %v", args)
		}
	})
}

func textStatement(wkt string, srid int32) (string, []any, error) {
	return orm.Compose(nil, orm.Project1(
		postgis.GeomFromText[orm.Composed](wkt, srid).Value(),
		func(g postgis.Geometry) postgis.Geometry { return g },
	)).From(oneRow).SQL()
}

// FuzzSpatialComposition builds a bounded composition of spatial operations
// from a byte string and checks the four invariants a statement must have,
// whatever it was built from.
func FuzzSpatialComposition(f *testing.F) {
	f.Add([]byte{0, 1, 2}, uint8(3))
	f.Add([]byte{5, 5, 5, 5, 5, 5}, uint8(1))
	f.Add([]byte{9, 8, 7, 6}, uint8(0))

	f.Fuzz(func(t *testing.T, ops []byte, which uint8) {
		if len(ops) > 24 {
			ops = ops[:24]
		}
		g := postgis.Value[orm.Composed](postgis.NewPolygon(4326, postgis.XY, square(0, 0, 10)))
		for _, op := range ops {
			g = applyOp(g, op)
		}

		var sql string
		var args []any
		var err error
		switch which % 3 {
		case 0:
			sql, args, err = orm.Compose(nil, orm.Project1(g.Value(),
				func(x postgis.Geometry) postgis.Geometry { return x })).From(oneRow).SQL()
		case 1:
			sql, args, err = orm.Compose(nil, orm.Project1(orm.Of(orm.BoolOf(
				g.Intersects(postgis.Value[orm.Composed](postgis.NewPoint(4326, 1, 1))))),
				func(b bool) bool { return b })).From(oneRow).SQL()
		default:
			sql, args, err = orm.Compose(nil, orm.Project1(g.AsText(),
				func(s string) string { return s })).From(oneRow).SQL()
		}
		if err != nil {
			// A refusal is a legitimate outcome — a Transform with no source
			// SRID, for instance — as long as it is a refusal and not a panic.
			return
		}

		// Deterministic: the same composition renders the same way.
		again, args2, err2 := func() (string, []any, error) {
			h := postgis.Value[orm.Composed](postgis.NewPolygon(4326, postgis.XY, square(0, 0, 10)))
			for _, op := range ops {
				h = applyOp(h, op)
			}
			switch which % 3 {
			case 0:
				return orm.Compose(nil, orm.Project1(h.Value(),
					func(x postgis.Geometry) postgis.Geometry { return x })).From(oneRow).SQL()
			case 1:
				return orm.Compose(nil, orm.Project1(orm.Of(orm.BoolOf(
					h.Intersects(postgis.Value[orm.Composed](postgis.NewPoint(4326, 1, 1))))),
					func(b bool) bool { return b })).From(oneRow).SQL()
			default:
				return orm.Compose(nil, orm.Project1(h.AsText(),
					func(s string) string { return s })).From(oneRow).SQL()
			}
		}()
		if err2 != nil || again != sql {
			t.Fatalf("the same composition rendered differently:\n%s\n%s\n%v", sql, again, err2)
		}
		if len(args) != len(args2) {
			t.Fatalf("the same composition bound %d and %d arguments", len(args), len(args2))
		}

		// The source is still named, so scope validation still has something to
		// check against.
		if !strings.Contains(sql, `"one_row"`) {
			t.Fatalf("the statement lost its source:\n%s", sql)
		}
		// Every geometry is still a parameter rather than text.
		if strings.Contains(sql, "POLYGON") || strings.Contains(sql, "0101000020") {
			t.Fatalf("a geometry reached the statement as text:\n%s", sql)
		}
	})
}

// applyOp is a small closed set of transformations, chosen by a byte.
func applyOp(g postgis.GeomExpr[orm.Composed], op byte) postgis.GeomExpr[orm.Composed] {
	other := postgis.Value[orm.Composed](postgis.NewPolygon(4326, postgis.XY, square(5, 5, 10)))
	switch op % 12 {
	case 0:
		return g.Buffer(1)
	case 1:
		return g.Centroid()
	case 2:
		return g.Envelope()
	case 3:
		return g.ConvexHull()
	case 4:
		return g.Intersection(other)
	case 5:
		return g.Union(other)
	case 6:
		return g.Difference(other)
	case 7:
		return g.Simplify(0.1)
	case 8:
		return g.SetSRID(4326)
	case 9:
		return g.Transform(3857).Transform(4326)
	case 10:
		return g.Multi()
	default:
		return g.MakeValid()
	}
}
