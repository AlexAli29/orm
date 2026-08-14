package postgis_test

import (
	"context"
	"strings"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/postgis"
	"github.com/jackc/pgx/v5/pgconn"
)

// The type matrix.
//
// Every public spatial expression in this package claims a Go result type. Each
// claim is checked here against what PostgreSQL actually says, and the check is
// two questions rather than one:
//
//	what base type does the server return          pg_typeof
//	what geometry does it actually hold            ST_SRID, GeometryType, ST_CoordDim
//
// The second matters because every PostGIS geometry is the same PostgreSQL base
// type. pg_typeof cannot tell a point from a polygon or 4326 from 3857, so an
// expression that claimed to preserve an SRID and quietly dropped it would pass
// a pg_typeof check and fail in production. Asking PostGIS what it really built
// is the only way to know.

// A geometry-valued expression, checked for what PostgreSQL returns and for
// what the geometry is.
type geomCase struct {
	name string
	// build renders the expression the package produces, so what is tested is
	// the SQL this package writes rather than SQL a test wrote.
	build func() (string, []any, error)
	// wantSRID, wantType and wantDims are what PostGIS should say about the
	// result. An empty wantType means the shape is not asserted.
	wantSRID int32
	wantType string
	wantDims int16
}

// oneRow is a source with exactly one row, which is what a composed query needs
// to select an expression that names no table.
//
// The alternative would be to build the SQL by hand, and then the test would be
// checking SQL a test wrote rather than SQL this package writes.
var oneRow = orm.NewSource("public", "one_row")

const oneRowDDL = `CREATE TABLE one_row (n int); INSERT INTO one_row VALUES (1);`

func exprSQL(v orm.Selectable[orm.Composed, postgis.Geometry]) func() (string, []any, error) {
	return func() (string, []any, error) {
		return orm.Compose(nil, orm.Project1(v, func(g postgis.Geometry) postgis.Geometry { return g })).
			From(oneRow).
			SQL()
	}
}

// base is a geometry in 4326 used as the operand for the transformations.
func base() postgis.GeomExpr[orm.Composed] {
	return postgis.Value[orm.Composed](postgis.NewPolygon(4326, postgis.XY, square(0, 0, 10)))
}

func point() postgis.GeomExpr[orm.Composed] {
	return postgis.Value[orm.Composed](postgis.NewPoint(4326, 1, 2))
}

func line() postgis.GeomExpr[orm.Composed] {
	return postgis.Value[orm.Composed](postgis.NewLineString(4326, postgis.XY,
		postgis.Coord{X: 0, Y: 0}, postgis.Coord{X: 5, Y: 5}))
}

// TestMatrix_geometryResults checks every geometry-valued expression: what it
// returns at the base-type level, and what PostGIS says the geometry is.
func TestMatrix_geometryResults(t *testing.T) {
	conn := gisConn(t)
	mustExec(t, conn, oneRowDDL)

	other := postgis.Value[orm.Composed](postgis.NewPolygon(4326, postgis.XY, square(5, 5, 10)))

	cases := []geomCase{
		{"MakeEnvelope", exprSQL(postgis.MakeEnvelope[orm.Composed](0, 0, 1, 1, 3857).Value()),
			3857, "ST_Polygon", 2},
		{"SetSRID", exprSQL(point().SetSRID(3857).Value()), 3857, "ST_Point", 2},
		{"Transform", exprSQL(point().Transform(3857).Value()), 3857, "ST_Point", 2},
		{"Buffer", exprSQL(point().Buffer(1).Value()), 4326, "ST_Polygon", 2},
		{"Centroid", exprSQL(base().Centroid().Value()), 4326, "ST_Point", 2},
		{"PointOnSurface", exprSQL(base().PointOnSurface().Value()), 4326, "ST_Point", 2},
		{"Envelope", exprSQL(base().Envelope().Value()), 4326, "ST_Polygon", 2},
		// The degenerate envelope: a point's envelope is a point, which is why
		// the result type is a general geometry rather than a polygon.
		{"Envelope of a point", exprSQL(point().Envelope().Value()), 4326, "ST_Point", 2},
		{"ConvexHull", exprSQL(base().ConvexHull().Value()), 4326, "ST_Polygon", 2},
		{"ConvexHull of a point", exprSQL(point().ConvexHull().Value()), 4326, "ST_Point", 2},
		{"Boundary", exprSQL(base().Boundary().Value()), 4326, "ST_LineString", 2},
		{"Intersection", exprSQL(base().Intersection(other).Value()), 4326, "ST_Polygon", 2},
		// Two polygons meeting along an edge intersect in a line, which is the
		// reason Intersection promises no shape.
		{"Intersection along an edge", exprSQL(base().Intersection(
			postgis.Value[orm.Composed](postgis.NewPolygon(4326, postgis.XY, square(10, 0, 10)))).Value()),
			4326, "ST_LineString", 2},
		{"Union", exprSQL(base().Union(other).Value()), 4326, "ST_Polygon", 2},
		{"Difference", exprSQL(base().Difference(other).Value()), 4326, "ST_Polygon", 2},
		{"SymDifference", exprSQL(base().SymDifference(other).Value()), 4326, "ST_MultiPolygon", 2},
		{"Simplify", exprSQL(line().Simplify(0.1).Value()), 4326, "ST_LineString", 2},
		{"SimplifyPreserveTopology", exprSQL(base().SimplifyPreserveTopology(0.1).Value()),
			4326, "ST_Polygon", 2},
		{"MakeValid", exprSQL(base().MakeValid().Value()), 4326, "ST_Polygon", 2},
		{"Multi", exprSQL(base().Multi().Value()), 4326, "ST_MultiPolygon", 2},
		{"Force3D", exprSQL(point().Force3D().Value()), 4326, "ST_Point", 3},
		{"Force2D of a Z point", exprSQL(
			postgis.Value[orm.Composed](postgis.NewPointZ(4326, 1, 2, 3)).Force2D().Value()),
			4326, "ST_Point", 2},
		{"GeomFromText", exprSQL(postgis.GeomFromText[orm.Composed]("POINT(1 2)", 4326).Value()),
			4326, "ST_Point", 2},
		{"GeomFromGeoJSON", exprSQL(postgis.GeomFromGeoJSON[orm.Composed](
			`{"type":"Point","coordinates":[1,2]}`).Value()), 4326, "ST_Point", 2},
		{"GeomFromEWKB", exprSQL(postgis.GeomFromEWKB[orm.Composed](
			postgis.NewPoint(4326, 1, 2).EWKB()).Value()), 4326, "ST_Point", 2},
		{"MakePoint", exprSQL(postgis.MakePoint(
			orm.Val(1.0), orm.Val(2.0)).SetSRID(4326).Value()), 4326, "ST_Point", 2},
		{"MakePointZ", exprSQL(postgis.MakePointZ(
			orm.Val(1.0), orm.Val(2.0), orm.Val(3.0)).SetSRID(4326).Value()), 4326, "ST_Point", 3},
		{"MakePointM", exprSQL(postgis.MakePointM(
			orm.Val(1.0), orm.Val(2.0), orm.Val(4.0)).SetSRID(4326).Value()), 4326, "ST_Point", 3},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sql, args, err := c.build()
			if err != nil {
				t.Fatalf("building the statement: %v", err)
			}
			// The statement selects one geometry; wrap it so the server reports
			// what that geometry is.
			inner := strings.TrimSuffix(sql, ";")
			q := `SELECT pg_typeof(g)::text, ST_SRID(g), ST_GeometryType(g), ST_CoordDim(g) FROM (` + inner + `) AS t(g)`

			var base, gtype string
			var srid int32
			var dims int16
			if err := conn.QueryRow(t.Context(), q, args...).Scan(&base, &srid, &gtype, &dims); err != nil {
				t.Fatalf("%s: %v\n%s", c.name, err, q)
			}
			if base != "geometry" {
				t.Errorf("the base type is %s, and this package reads it as a geometry", base)
			}
			if srid != c.wantSRID {
				t.Errorf("PostGIS says SRID %d; this package claims %d", srid, c.wantSRID)
			}
			// ST_GeometryType rather than GeometryType: the first spells the
			// shape one way for every dimensionality, and the second appends M
			// for XYM and nothing for XYZ, which is an inconsistency in PostGIS
			// that a test should not have to encode twice.
			if c.wantType != "" && gtype != c.wantType {
				t.Errorf("PostGIS built a %s; the test expected %s", gtype, c.wantType)
			}
			if dims != int16(c.wantDims) {
				t.Errorf("PostGIS says %d dimensions; the test expected %d", dims, c.wantDims)
			}
		})
	}
}

// The scalar-valued expressions, checked against pg_typeof.
//
// Each entry names the Go type this package claims, and the test asserts the
// server's answer maps to it. A claim that is merely plausible is a claim
// nobody checked.
func TestMatrix_scalarResults(t *testing.T) {
	conn := gisConn(t)
	mustExec(t, conn, oneRowDDL)

	tests := []struct {
		name   string
		build  func() (string, []any, error)
		goType string
		pgType string
	}{
		{"Area", scalarSQL(base().Area()), "float64", "double precision"},
		{"Length", scalarSQL(line().Length()), "float64", "double precision"},
		{"Perimeter", scalarSQL(base().Perimeter()), "float64", "double precision"},
		{"Distance", scalarSQL(base().Distance(point())), "float64", "double precision"},
		{"MaxDistance", scalarSQL(base().MaxDistance(point())), "float64", "double precision"},
		{"KNNDistance", scalarSQL(base().KNNDistance(point())), "float64", "double precision"},
		{"BBoxDistance", scalarSQL(base().BBoxDistance(point())), "float64", "double precision"},
		{"SRID", scalarSQL(base().SRID()), "int32", "integer"},
		{"NumPoints", scalarSQL(base().NumPoints()), "int32", "integer"},
		{"NumGeometries", scalarSQL(base().NumGeometries()), "int32", "integer"},
		{"Dimension", scalarSQL(base().Dimension()), "int32", "integer"},
		{"CoordDim", scalarSQL(base().CoordDim()), "int16", "smallint"},
		{"GeometryType", scalarSQL(base().GeometryType()), "string", "text"},
		{"IsValidReason", scalarSQL(base().IsValidReason()), "string", "text"},
		{"AsText", scalarSQL(base().AsText()), "string", "text"},
		{"AsEWKT", scalarSQL(base().AsEWKT()), "string", "text"},
		{"AsGeoJSON", scalarSQL(base().AsGeoJSON()), "string", "text"},
		{"AsBinary", scalarSQL(base().AsBinary()), "[]byte", "bytea"},
		{"AsEWKB", scalarSQL(base().AsEWKB()), "[]byte", "bytea"},
		{"X", nullScalarSQL(point().X()), "*float64", "double precision"},
		{"Y", nullScalarSQL(point().Y()), "*float64", "double precision"},
		{"Z", nullScalarSQL(postgis.Value[orm.Composed](
			postgis.NewPointZ(4326, 1, 2, 3)).Z()), "*float64", "double precision"},
		{"M", nullScalarSQL(postgis.Value[orm.Composed](
			postgis.NewPointM(4326, 1, 2, 4)).M()), "*float64", "double precision"},
		{"Azimuth", nullScalarSQL(point().Azimuth(
			postgis.Value[orm.Composed](postgis.NewPoint(4326, 5, 5)))), "*float64", "double precision"},
		{"geography Area", scalarSQL(geogBase().Area()), "float64", "double precision"},
		{"geography Length", scalarSQL(geogLine().Length()), "float64", "double precision"},
		{"geography Distance", scalarSQL(geogBase().Distance(geogLine())), "float64", "double precision"},
		{"geography AsGeoJSON", scalarSQL(geogBase().AsGeoJSON()), "string", "text"},
		{"geography SRID", scalarSQL(geogBase().SRID()), "int32", "integer"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sql, args, err := tc.build()
			if err != nil {
				t.Fatalf("building the statement: %v", err)
			}
			inner := strings.TrimSuffix(sql, ";")
			var got string
			q := `SELECT pg_typeof(v)::text FROM (` + inner + `) AS t(v)`
			if err := conn.QueryRow(t.Context(), q, args...).Scan(&got); err != nil {
				t.Fatalf("%s: %v\n%s", tc.name, err, q)
			}
			if got != tc.pgType {
				t.Errorf("PostgreSQL returns %s; this package claims %s (%s)", got, tc.pgType, tc.goType)
			}
		})
	}
}

func scalarSQL[T any](v orm.Selectable[orm.Composed, T]) func() (string, []any, error) {
	return func() (string, []any, error) {
		return orm.Compose(nil, orm.Project1(v, func(x T) T { return x })).From(oneRow).SQL()
	}
}

func nullScalarSQL[T any](v orm.Selectable[orm.Composed, *T]) func() (string, []any, error) {
	return func() (string, []any, error) {
		return orm.Compose(nil, orm.Project1(v, func(x *T) *T { return x })).From(oneRow).SQL()
	}
}

func geogBase() postgis.GeogExpr[orm.Composed] {
	g, err := postgis.NewPolygon(4326, postgis.XY, square(0, 0, 1)).AsGeography()
	if err != nil {
		panic(err)
	}
	return postgis.GeogValue[orm.Composed](g)
}

func geogLine() postgis.GeogExpr[orm.Composed] {
	g, err := postgis.NewLineString(4326, postgis.XY,
		postgis.Coord{X: 0, Y: 0}, postgis.Coord{X: 1, Y: 1}).AsGeography()
	if err != nil {
		panic(err)
	}
	return postgis.GeogValue[orm.Composed](g)
}

// The predicates all return boolean, which is the one claim they make.
func TestMatrix_predicateResults(t *testing.T) {
	conn := gisConn(t)
	mustExec(t, conn, oneRowDDL)

	other := postgis.Value[orm.Composed](postgis.NewPolygon(4326, postgis.XY, square(5, 5, 10)))
	preds := map[string]orm.Predicate[orm.Composed]{
		"Intersects":       base().Intersects(other),
		"Disjoint":         base().Disjoint(other),
		"Contains":         base().Contains(other),
		"ContainsProperly": base().ContainsProperly(other),
		"Within":           base().Within(other),
		"Covers":           base().Covers(other),
		"CoveredBy":        base().CoveredBy(other),
		"Overlaps":         base().Overlaps(other),
		"Touches":          base().Touches(other),
		"Crosses":          line().Crosses(other),
		"EqualsGeom":       base().EqualsGeom(other),
		"DWithin":          base().DWithin(other, 1),
		"DFullyWithin":     base().DFullyWithin(other, 1000),
		"BBoxIntersects":   base().BBoxIntersects(other),
		"BBoxContains":     base().BBoxContains(other),
		"BBoxWithin":       base().BBoxWithin(other),
		"BBoxSame":         base().BBoxSame(other),
		"Relate":           base().Relate(other, "T********"),
		"IsValid":          base().IsValid(),
		"IsEmptyGeom":      base().IsEmptyGeom(),
		"IsSimple":         base().IsSimple(),
		"geog Intersects":  geogBase().Intersects(geogLine()),
		"geog Covers":      geogBase().Covers(geogLine()),
		"geog CoveredBy":   geogBase().CoveredBy(geogLine()),
		"geog DWithin":     geogBase().DWithin(geogLine(), 1000),
		"geog BBox":        geogBase().BBoxIntersects(geogLine()),
	}

	// Sorted so the subtests run in a fixed order.
	names := make([]string, 0, len(preds))
	for name := range preds {
		names = append(names, name)
	}
	sortStrings(names)

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			sql, args, err := orm.Compose(nil,
				orm.Project1(orm.Of(orm.BoolOf(preds[name])), func(b bool) bool { return b })).
				From(oneRow).SQL()
			if err != nil {
				t.Fatalf("building the statement: %v", err)
			}
			inner := strings.TrimSuffix(sql, ";")
			var got string
			q := `SELECT pg_typeof(v)::text FROM (` + inner + `) AS t(v)`
			if err := conn.QueryRow(t.Context(), q, args...).Scan(&got); err != nil {
				t.Fatalf("%s: %v\n%s", name, err, q)
			}
			if got != "boolean" {
				t.Errorf("%s returns %s", name, got)
			}
		})
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func mustExec(t *testing.T, conn interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}, sql string) {
	t.Helper()
	if _, err := conn.Exec(t.Context(), sql); err != nil {
		t.Fatalf("running %s: %v", sql, err)
	}
}
