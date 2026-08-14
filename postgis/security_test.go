package postgis_test

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/postgis"
)

// The security corpus.
//
// The rule this package holds to is the rule the rest of the ORM holds to:
// values are bind parameters and never text, identifiers and operator spellings
// come from closed sets inside this package, and nothing a caller passes
// reaches the statement as syntax.
//
// Spatial input is where that rule is easiest to break, because WKT and GeoJSON
// are text and text is tempting to concatenate. So the corpus below sends the
// shapes that would break a formatter — quotes, backslashes, comment markers, a
// whole statement — and asserts two things: the SQL this package builds does not
// change, and every one of those payloads is in the argument list instead.

// hostile is text that would end a string literal, start a comment, or run a
// statement, if any of it were ever formatted into SQL.
var hostile = []string{
	`POINT(1 2)'); DROP TABLE places; --`,
	`'`,
	`''`,
	`\`,
	`\\'`,
	`"; DROP TABLE places; --`,
	`POINT(1 2) -- comment`,
	`/* comment */ POINT(1 2)`,
	"POINT(1 2)\x00",
	"POINT(1 2)\n;\nDROP TABLE places",
	`ПУНКТ(1 2)`,
	`POINT(1 2)😀`,
	strings.Repeat("A", 100_000),
	`{"type":"Point","coordinates":[1,2]}'); DROP TABLE places; --`,
	`{"type":"Point"`,
	`not json at all`,
}

// TestSecurity_wktIsAParameter asserts the statement is invariant under the
// payload: whatever the text is, the SQL is the same and the text is an
// argument.
func TestSecurity_wktIsAParameter(t *testing.T) {
	var baseline string
	for i, payload := range hostile {
		sql, args, err := orm.Compose(nil, orm.Project1(
			postgis.GeomFromText[orm.Composed](payload, 4326).Value(),
			func(g postgis.Geometry) postgis.Geometry { return g },
		)).From(oneRow).SQL()
		if err != nil {
			t.Fatalf("payload %d: %v", i, err)
		}
		if i == 0 {
			baseline = sql
		} else if sql != baseline {
			t.Errorf("payload %d changed the statement:\n%s\nwant\n%s", i, sql, baseline)
		}
		if !bound(args, payload) {
			t.Errorf("payload %d did not reach the argument list: %v", i, args)
		}
		if strings.Contains(sql, "DROP") || strings.Contains(sql, payload[:min(len(payload), 8)]) {
			t.Errorf("payload %d appears in the statement:\n%s", i, sql)
		}
	}
}

// The same for GeoJSON, which is the other text input.
func TestSecurity_geoJSONIsAParameter(t *testing.T) {
	var baseline string
	for i, payload := range hostile {
		sql, args, err := orm.Compose(nil, orm.Project1(
			postgis.GeomFromGeoJSON[orm.Composed](payload).Value(),
			func(g postgis.Geometry) postgis.Geometry { return g },
		)).From(oneRow).SQL()
		if err != nil {
			t.Fatalf("payload %d: %v", i, err)
		}
		if i == 0 {
			baseline = sql
		} else if sql != baseline {
			t.Errorf("payload %d changed the statement:\n%s", i, sql)
		}
		if !bound(args, payload) {
			t.Errorf("payload %d did not reach the argument list", i)
		}
	}
}

// And for the geography parser.
func TestSecurity_geogTextIsAParameter(t *testing.T) {
	var baseline string
	for i, payload := range hostile {
		sql, args, err := orm.Compose(nil, orm.Project1(
			postgis.GeogFromText[orm.Composed](payload).Value(),
			func(g postgis.Geography) postgis.Geography { return g },
		)).From(oneRow).SQL()
		if err != nil {
			t.Fatalf("payload %d: %v", i, err)
		}
		if i == 0 {
			baseline = sql
		} else if sql != baseline {
			t.Errorf("payload %d changed the statement", i)
		}
		if !bound(args, payload) {
			t.Errorf("payload %d did not reach the argument list", i)
		}
	}
}

// Coordinates are numbers and are still parameters. A formatter would have to
// render a float, and rendering a float is where NaN, infinity and precision
// go wrong.
func TestSecurity_coordinatesAreParameters(t *testing.T) {
	var baseline string
	for i, pt := range []postgis.Geometry{
		postgis.NewPoint(4326, 1, 2),
		postgis.NewPoint(4326, 1e308, -1e308),
		postgis.NewPoint(4326, 0.1+0.2, 5e-324),
	} {
		sql, args, err := orm.Compose(nil, orm.Project1(
			postgis.Value[orm.Composed](pt).Value(),
			func(g postgis.Geometry) postgis.Geometry { return g },
		)).From(oneRow).SQL()
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			baseline = sql
		} else if sql != baseline {
			t.Errorf("a different coordinate changed the statement:\n%s", sql)
		}
		var seen bool
		for _, a := range args {
			if _, ok := a.(postgis.Geometry); ok {
				seen = true
			}
		}
		if !seen {
			t.Errorf("the geometry did not reach the argument list: %v", args)
		}
	}
}

// The SRID is an integer in a parameter position, and the ones that would be
// dangerous as text are refused as numbers before they get anywhere.
func TestSecurity_sridIsAnInteger(t *testing.T) {
	// The type is int32, so a string SRID does not compile. What can still go
	// wrong is a nonsense number, and each of these is refused while the query
	// is being built.
	for _, srid := range []int32{-1, 0, -4326} {
		_, _, err := orm.Compose(nil, orm.Project1(
			postgis.Value[orm.Composed](postgis.NewPoint(4326, 1, 2)).Transform(srid).Value(),
			func(g postgis.Geometry) postgis.Geometry { return g },
		)).From(oneRow).SQL()
		if err == nil {
			t.Errorf("ST_Transform to SRID %d built a statement", srid)
		}
	}
	// And a valid one is a bind parameter rather than a literal.
	sql, args, err := orm.Compose(nil, orm.Project1(
		postgis.Value[orm.Composed](postgis.NewPoint(4326, 1, 2)).Transform(3857).Value(),
		func(g postgis.Geometry) postgis.Geometry { return g },
	)).From(oneRow).SQL()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sql, "3857") {
		t.Errorf("the SRID was written into the statement:\n%s", sql)
	}
	if !bound(args, int32(3857)) {
		t.Errorf("the SRID is not in the argument list: %v", args)
	}
}

// The DE-9IM pattern is validated against a closed alphabet and then bound.
//
// It is the one argument in this package that is a string a caller chooses and
// that PostGIS interprets, so it gets both: a check that it is a pattern, and a
// parameter so that being one is not load-bearing for safety.
func TestSecurity_relatePattern(t *testing.T) {
	bad := []string{
		"", "TTT", "T*******X", "'; DROP TABLE places; --",
		"TTTTTTTTTT", "T********'", strings.Repeat("*", 100),
	}
	for _, pattern := range bad {
		_, _, err := orm.Compose(nil, orm.Project1(
			orm.Of(orm.BoolOf(base().Relate(point(), pattern))),
			func(b bool) bool { return b },
		)).From(oneRow).SQL()
		if err == nil {
			t.Errorf("the pattern %q built a statement", pattern)
		}
	}

	sql, args, err := orm.Compose(nil, orm.Project1(
		orm.Of(orm.BoolOf(base().Relate(point(), "T********"))),
		func(b bool) bool { return b },
	)).From(oneRow).SQL()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sql, "T********") {
		t.Errorf("the pattern was written into the statement:\n%s", sql)
	}
	if !bound(args, "T********") {
		t.Errorf("the pattern is not in the argument list: %v", args)
	}
}

// A malformed geometry is PostGIS's error rather than a statement that ran.
func TestSecurity_malformedInputIsAnError(t *testing.T) {
	conn := gisConn(t)
	mustExec(t, conn, oneRowDDL)
	mustExec(t, conn, `CREATE TABLE canary (n int)`)

	for _, payload := range hostile {
		sql, args, err := orm.Compose(nil, orm.Project1(
			postgis.GeomFromText[orm.Composed](payload, 4326).Value(),
			func(g postgis.Geometry) postgis.Geometry { return g },
		)).From(oneRow).SQL()
		if err != nil {
			t.Fatalf("building the statement: %v", err)
		}
		// Errors are expected and are the point; what matters is that nothing
		// ran that should not have.
		_, _ = conn.Exec(t.Context(), sql, args...)
	}

	var exists bool
	if err := conn.QueryRow(t.Context(),
		`SELECT to_regclass('canary') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("a payload dropped a table, so something reached the statement as syntax")
	}
}

// Nothing in this package panics or allocates without bound on hostile input.
func TestSecurity_hostileDecodeDoesNotPanic(t *testing.T) {
	inputs := [][]byte{
		nil, {}, {1}, {1, 1}, {0xff}, {1, 0, 0, 0, 0},
		{1, 1, 0, 0, 0x20}, // an SRID flag with no SRID
		{1, 0x07, 0, 0, 0, 0xff, 0xff, 0xff, 0x7f},
		{1, 0x02, 0, 0, 0, 0xff, 0xff, 0xff, 0x7f},
		make([]byte, 1024),
	}
	for i, raw := range inputs {
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("input %d panicked: %v", i, p)
				}
			}()
			_, _ = postgis.DecodeEWKB(raw)
		}()
	}
}

// ParseTypeMod is the other parser a caller's text reaches, through a struct
// tag. It has to refuse rather than panic on anything.
func TestSecurity_parseTypeModIsTotal(t *testing.T) {
	inputs := []string{
		"", "geometry", "geometry(", "geometry()", "geometry(Point",
		"geometry(Point,4326", "geometry(Point,4326))", "geometry(Point,not-a-number)",
		"geometry(Nonsense,4326)", "geometry(Point,4326,extra)",
		"geometry(Point,99999999999999999999)", "geography(POINTZM,4326)",
		"GEOMETRY(POINT,4326)", strings.Repeat("geometry(", 1000),
		"'; DROP TABLE places; --", "geometry(Point,4326); DROP TABLE places",
	}
	for _, in := range inputs {
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("%q panicked: %v", in, p)
				}
			}()
			m, err := postgis.ParseTypeMod(in)
			if err == nil && m.Spatial() {
				// A parse that succeeded has to round-trip, or the canonical
				// form a migration compares is not canonical.
				again, err2 := postgis.ParseTypeMod(m.String())
				if err2 != nil || again.String() != m.String() {
					t.Errorf("%q parsed to %q, which does not round-trip (%v)", in, m, err2)
				}
			}
		}()
	}
}

// bound reports whether a value reached the statement's argument list.
func bound(args []any, want any) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
