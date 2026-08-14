package postgis_test

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/postgis"
)

// Tests that exist because a mutation survived without them.
//
// Mutation testing applies one deliberate defect and asks whether the suite
// notices. Everything below closes a mutation that got through: the metadata
// that nothing read, the operator nobody asserted, the shape claim no test
// compared against the server. Each of these would have been a silent
// regression.

// The dimensionality a descriptor carries is metadata nothing else reads, so
// resetting it to XY survived every other test. It is what a future check would
// read, and what the generator's output is asserted against, so it has to be
// asserted here too.
func TestMutation_dimensionalityMetadataIsCarried(t *testing.T) {
	src := orm.NewSource("public", "readings")

	for _, tc := range []struct {
		name string
		dim  postgis.Dim
	}{
		{"XY", postgis.XY}, {"XYZ", postgis.XYZ},
		{"XYM", postgis.XYM}, {"XYZM", postgis.XYZM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			col := postgis.NewGeomCol[audited](src, "g", 4326, postgis.KindPoint, tc.dim)
			if got := col.Dim(); got != tc.dim {
				t.Errorf("the descriptor reports %v", got)
			}
			// And through the expression, where a chain could drop it.
			if got := col.Expr().DeclaredDim(); got != tc.dim {
				t.Errorf("the expression reports %v", got)
			}
			// A transformation keeps the dimensionality, because reprojecting
			// does not add or remove an ordinate.
			if got := col.Expr().Transform(3857).DeclaredDim(); got != tc.dim {
				t.Errorf("after Transform the expression reports %v", got)
			}
			if got := col.Expr().SetSRID(3857).DeclaredDim(); got != tc.dim {
				t.Errorf("after SetSRID the expression reports %v", got)
			}
			if got := col.Expr().Centroid().DeclaredDim(); got != tc.dim {
				t.Errorf("after Centroid the expression reports %v", got)
			}
			// Force2D and Force3D are the two that change it, and they say so.
			if got := col.Expr().Force2D().DeclaredDim(); got != postgis.XY {
				t.Errorf("Force2D reports %v", got)
			}
			if got := col.Expr().Force3D().DeclaredDim(); got != postgis.XYZ {
				t.Errorf("Force3D reports %v", got)
			}
		})
	}

	// The geography descriptors carry it too.
	geog := postgis.NewGeogCol[audited](src, "g", 4326, postgis.KindPoint, postgis.XYZ)
	if got := geog.Dim(); got != postgis.XYZ {
		t.Errorf("the geography descriptor reports %v", got)
	}
	nullGeom := postgis.NewNullGeomCol[audited](src, "g", 4326, postgis.KindPoint, postgis.XYM)
	if got := nullGeom.Expr().DeclaredDim(); got != postgis.XYM {
		t.Errorf("the nullable descriptor reports %v", got)
	}
}

// The shape metadata each transformation claims, asserted against what PostGIS
// actually produces for a degenerate input.
//
// Declaring Envelope or ConvexHull a Polygon survived, because nothing compared
// the claim with the server for an input whose answer is not a polygon. These
// are those inputs.
func TestMutation_shapeClaimsMatchTheServer(t *testing.T) {
	conn := gisConn(t)
	mustExec(t, conn, oneRowDDL)

	pt := postgis.Value[orm.Composed](postgis.NewPoint(4326, 1, 2))
	ln := postgis.Value[orm.Composed](postgis.NewLineString(4326, postgis.XY,
		postgis.Coord{X: 0, Y: 0}, postgis.Coord{X: 4, Y: 0}))
	sq := postgis.Value[orm.Composed](postgis.NewPolygon(4326, postgis.XY, square(0, 0, 4)))
	// A square sharing only an edge with the first, so their intersection is a
	// line rather than a polygon.
	edge := postgis.Value[orm.Composed](postgis.NewPolygon(4326, postgis.XY, square(4, 0, 4)))
	// And one sharing only a corner, so their intersection is a point.
	corner := postgis.Value[orm.Composed](postgis.NewPolygon(4326, postgis.XY, square(4, 4, 4)))
	far := postgis.Value[orm.Composed](postgis.NewPolygon(4326, postgis.XY, square(50, 50, 4)))

	cases := []struct {
		name string
		expr postgis.GeomExpr[orm.Composed]
		// server is what PostGIS builds. claim is what the expression says its
		// shape is, and the two are compared through the rule below.
		server string
	}{
		{"Envelope of a point", pt.Envelope(), "ST_Point"},
		{"Envelope of a horizontal line", ln.Envelope(), "ST_LineString"},
		{"Envelope of a square", sq.Envelope(), "ST_Polygon"},
		{"ConvexHull of a point", pt.ConvexHull(), "ST_Point"},
		{"ConvexHull of a line", ln.ConvexHull(), "ST_LineString"},
		{"ConvexHull of a square", sq.ConvexHull(), "ST_Polygon"},
		{"Intersection along an edge", sq.Intersection(edge), "ST_LineString"},
		{"Intersection at a corner", sq.Intersection(corner), "ST_Point"},
		{"Intersection of two overlapping squares", sq.Intersection(
			postgis.Value[orm.Composed](postgis.NewPolygon(4326, postgis.XY, square(2, 2, 4)))), "ST_Polygon"},
		{"Intersection of disjoint squares", sq.Intersection(far), "ST_Polygon"},
		{"Union of disjoint squares", sq.Union(far), "ST_MultiPolygon"},
		{"Union of a point and a line", pt.Union(ln), "ST_GeometryCollection"},
		{"Difference leaving nothing", sq.Difference(sq), "ST_Polygon"},
		{"SymDifference of disjoint squares", sq.SymDifference(far), "ST_MultiPolygon"},
		{"Boundary of a square", sq.Boundary(), "ST_LineString"},
		{"Boundary of a line", ln.Boundary(), "ST_MultiPoint"},
		{"MakeValid of a bow tie", postgis.Value[orm.Composed](postgis.NewPolygon(
			4326, postgis.XY, []postgis.Coord{
				{X: 0, Y: 0}, {X: 4, Y: 4}, {X: 4, Y: 0}, {X: 0, Y: 4}, {X: 0, Y: 0},
			})).MakeValid(), "ST_MultiPolygon"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sql, args, err := orm.Compose(nil, orm.Project1(c.expr.Value(),
				func(g postgis.Geometry) postgis.Geometry { return g })).From(oneRow).SQL()
			if err != nil {
				t.Fatalf("building: %v", err)
			}
			inner := strings.TrimSuffix(sql, ";")
			var gtype string
			q := `SELECT ST_GeometryType(g) FROM (` + inner + `) AS t(g)`
			if err := conn.QueryRow(t.Context(), q, args...).Scan(&gtype); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			if gtype != c.server {
				t.Fatalf("PostGIS built a %s; the corpus expected %s", gtype, c.server)
			}

			// The expression's own claim has to be one PostGIS can keep. Either
			// it claims nothing, or what it claims is what came back.
			claim := c.expr.DeclaredKind()
			if claim == postgis.AnyKind {
				return
			}
			if "ST_"+claim.String() != gtype {
				t.Errorf("the expression claims %s and PostGIS built a %s", claim, gtype)
			}
		})
	}

	// And the corpus really does contain inputs that refute a narrower claim,
	// so that declaring these Polygon cannot pass.
	refuting := map[string]int{}
	for _, c := range cases {
		if strings.HasPrefix(c.name, "Envelope") && c.server != "ST_Polygon" {
			refuting["Envelope"]++
		}
		if strings.HasPrefix(c.name, "ConvexHull") && c.server != "ST_Polygon" {
			refuting["ConvexHull"]++
		}
		if strings.HasPrefix(c.name, "Intersection") && c.server != "ST_Polygon" {
			refuting["Intersection"]++
		}
		if strings.HasPrefix(c.name, "Union") && c.server != "ST_Polygon" {
			refuting["Union"]++
		}
	}
	for _, fn := range []string{"Envelope", "ConvexHull", "Intersection", "Union"} {
		if refuting[fn] == 0 {
			t.Errorf("no case refutes a narrow shape claim for %s", fn)
		}
	}
}

// The KNN operator has to be the operator.
//
// Rewriting <-> to ST_Distance gives the same ordering on this data and a
// completely different plan, so no result-comparing test can catch it. The
// statement is what has to be asserted.
func TestMutation_knnUsesTheOperator(t *testing.T) {
	g := postgis.Value[orm.Composed](postgis.NewPolygon(4326, postgis.XY, square(0, 0, 10)))
	other := postgis.Value[orm.Composed](postgis.NewPoint(4326, 1, 1))

	for _, tc := range []struct {
		name string
		v    orm.Selectable[orm.Composed, float64]
		op   string
	}{
		{"KNNDistance", g.KNNDistance(other), "<->"},
		{"BBoxDistance", g.BBoxDistance(other), "<#>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sql, _, err := orm.Compose(nil, orm.Project1(tc.v,
				func(d float64) float64 { return d })).From(oneRow).SQL()
			if err != nil {
				t.Fatalf("building: %v", err)
			}
			if !strings.Contains(sql, tc.op) {
				t.Errorf("the statement does not use %s:\n%s", tc.op, sql)
			}
			if strings.Contains(sql, "ST_Distance") {
				t.Errorf("the statement was rewritten to a function call:\n%s", sql)
			}
		})
	}

	// The bounding-box predicates use their operators too.
	for _, tc := range []struct {
		name string
		p    orm.Predicate[orm.Composed]
		op   string
	}{
		{"BBoxIntersects", g.BBoxIntersects(other), "&&"},
		{"BBoxContains", g.BBoxContains(other), "~"},
		{"BBoxWithin", g.BBoxWithin(other), "@"},
		{"BBoxSame", g.BBoxSame(other), "~="},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sql, _, err := orm.Compose(nil, orm.Project1(orm.Of(orm.BoolOf(tc.p)),
				func(b bool) bool { return b })).From(oneRow).SQL()
			if err != nil {
				t.Fatalf("building: %v", err)
			}
			if !strings.Contains(sql, tc.op) {
				t.Errorf("the statement does not use %s:\n%s", tc.op, sql)
			}
			if strings.Contains(sql, "ST_") {
				t.Errorf("a bounding-box operator became a function call:\n%s", sql)
			}
		})
	}
}

// Every predicate has to call the function it is named after.
//
// Within and CoveredBy survived being replaced by Intersects and Within because
// the row-level corpus did not separate them everywhere. The function name is
// what the API promises, so that is what is asserted — and the differential
// corpus in the semantics tests proves the names mean different things.
func TestMutation_predicatesCallTheirOwnFunction(t *testing.T) {
	a := postgis.Value[orm.Composed](postgis.NewPolygon(4326, postgis.XY, square(0, 0, 10)))
	b := postgis.Value[orm.Composed](postgis.NewPoint(4326, 1, 1))
	ga, _ := postgis.NewPolygon(4326, postgis.XY, square(0, 0, 1)).AsGeography()
	gb, _ := postgis.NewPoint(4326, 0.5, 0.5).AsGeography()
	geogA := postgis.GeogValue[orm.Composed](ga)
	geogB := postgis.GeogValue[orm.Composed](gb)

	cases := []struct {
		fn string
		p  orm.Predicate[orm.Composed]
	}{
		{"ST_Intersects", a.Intersects(b)},
		{"ST_Disjoint", a.Disjoint(b)},
		{"ST_Contains", a.Contains(b)},
		{"ST_ContainsProperly", a.ContainsProperly(b)},
		{"ST_Within", a.Within(b)},
		{"ST_Covers", a.Covers(b)},
		{"ST_CoveredBy", a.CoveredBy(b)},
		{"ST_Overlaps", a.Overlaps(b)},
		{"ST_Touches", a.Touches(b)},
		{"ST_Crosses", a.Crosses(b)},
		{"ST_Equals", a.EqualsGeom(b)},
		{"ST_DWithin", a.DWithin(b, 1)},
		{"ST_DFullyWithin", a.DFullyWithin(b, 1)},
		{"ST_Relate", a.Relate(b, "T********")},
		{"ST_IsValid", a.IsValid()},
		{"ST_IsEmpty", a.IsEmptyGeom()},
		{"ST_IsSimple", a.IsSimple()},
		{"ST_Intersects", geogA.Intersects(geogB)},
		{"ST_Covers", geogA.Covers(geogB)},
		{"ST_CoveredBy", geogA.CoveredBy(geogB)},
		{"ST_DWithin", geogA.DWithin(geogB, 1)},
	}
	for _, c := range cases {
		sql, _, err := orm.Compose(nil, orm.Project1(orm.Of(orm.BoolOf(c.p)),
			func(b bool) bool { return b })).From(oneRow).SQL()
		if err != nil {
			t.Fatalf("%s: %v", c.fn, err)
		}
		if !strings.Contains(sql, c.fn+"(") {
			t.Errorf("the predicate does not call %s:\n%s", c.fn, sql)
		}
	}
}

// And every value-producing spatial function, for the same reason.
func TestMutation_valuesCallTheirOwnFunction(t *testing.T) {
	g := postgis.Value[orm.Composed](postgis.NewPolygon(4326, postgis.XY, square(0, 0, 10)))
	pt := postgis.Value[orm.Composed](postgis.NewPoint(4326, 1, 2))

	geoms := map[string]postgis.GeomExpr[orm.Composed]{
		"ST_Buffer":                   g.Buffer(1),
		"ST_Centroid":                 g.Centroid(),
		"ST_PointOnSurface":           g.PointOnSurface(),
		"ST_Envelope":                 g.Envelope(),
		"ST_ConvexHull":               g.ConvexHull(),
		"ST_Boundary":                 g.Boundary(),
		"ST_Intersection":             g.Intersection(pt),
		"ST_Union":                    g.Union(pt),
		"ST_Difference":               g.Difference(pt),
		"ST_SymDifference":            g.SymDifference(pt),
		"ST_Simplify":                 g.Simplify(0.1),
		"ST_SimplifyPreserveTopology": g.SimplifyPreserveTopology(0.1),
		"ST_MakeValid":                g.MakeValid(),
		"ST_Force2D":                  g.Force2D(),
		"ST_Force3D":                  g.Force3D(),
		"ST_Multi":                    g.Multi(),
		"ST_CollectionExtract":        g.CollectionExtract(postgis.KindPolygon),
		"ST_SetSRID":                  g.SetSRID(4326),
		"ST_Transform":                g.Transform(3857),
	}
	for fn, e := range geoms {
		sql, _, err := orm.Compose(nil, orm.Project1(e.Value(),
			func(x postgis.Geometry) postgis.Geometry { return x })).From(oneRow).SQL()
		if err != nil {
			t.Fatalf("%s: %v", fn, err)
		}
		if !strings.Contains(sql, fn+"(") {
			t.Errorf("the expression does not call %s:\n%s", fn, sql)
		}
	}

	scalars := map[string]func() (string, []any, error){
		"ST_Area":          scalarSQL(g.Area()),
		"ST_Length":        scalarSQL(g.Length()),
		"ST_Perimeter":     scalarSQL(g.Perimeter()),
		"ST_Distance":      scalarSQL(g.Distance(pt)),
		"ST_MaxDistance":   scalarSQL(g.MaxDistance(pt)),
		"ST_SRID":          scalarSQL(g.SRID()),
		"ST_NPoints":       scalarSQL(g.NumPoints()),
		"ST_NumGeometries": scalarSQL(g.NumGeometries()),
		"ST_Dimension":     scalarSQL(g.Dimension()),
		"ST_GeometryType":  scalarSQL(g.GeometryType()),
		"ST_IsValidReason": scalarSQL(g.IsValidReason()),
		"ST_AsText":        scalarSQL(g.AsText()),
		"ST_AsEWKT":        scalarSQL(g.AsEWKT()),
		"ST_AsGeoJSON":     scalarSQL(g.AsGeoJSON()),
		"ST_AsBinary":      scalarSQL(g.AsBinary()),
		"ST_AsEWKB":        scalarSQL(g.AsEWKB()),
		"ST_X":             nullScalarSQL(pt.X()),
		"ST_Y":             nullScalarSQL(pt.Y()),
		"ST_Z":             nullScalarSQL(pt.Z()),
		"ST_M":             nullScalarSQL(pt.M()),
		"ST_Azimuth":       nullScalarSQL(pt.Azimuth(g)),
	}
	for fn, build := range scalars {
		sql, _, err := build()
		if err != nil {
			t.Fatalf("%s: %v", fn, err)
		}
		if !strings.Contains(sql, fn+"(") {
			t.Errorf("the expression does not call %s:\n%s", fn, sql)
		}
	}

	// ST_CoordDim renders separately because its result type is the one PostGIS
	// spells differently from every other counting function.
	sql, _, err := orm.Compose(nil, orm.Project1(g.CoordDim(),
		func(d int16) int16 { return d })).From(oneRow).SQL()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, "ST_CoordDim(") {
		t.Errorf("CoordDim does not call ST_CoordDim:\n%s", sql)
	}
}

// Every scalar argument that PostgreSQL uses to resolve an overload has to
// reach the statement with a type.
//
// Removing the cast from ST_SetSRID survived because no test forced a bare
// parameter through it, and the same mistake in ST_Transform was a real bug
// once. The rendered statement is what proves the cast is there.
func TestMutation_overloadArgumentsAreCast(t *testing.T) {
	g := postgis.Value[orm.Composed](postgis.NewPoint(4326, 1, 2))

	cases := []struct {
		name string
		sql  func() (string, []any, error)
		want string
	}{
		{"SetSRID", exprSQL(g.SetSRID(3857).Value()), `CAST($2 AS "int4")`},
		{"Transform", exprSQL(g.Transform(3857).Value()), `CAST($2 AS "int4")`},
		{"MakeEnvelope", exprSQL(
			postgis.MakeEnvelope[orm.Composed](0, 0, 1, 1, 4326).Value()), `CAST($5 AS "int4")`},
		{"GeomFromText", exprSQL(
			postgis.GeomFromText[orm.Composed]("POINT(1 2)", 4326).Value()), `CAST($2 AS "int4")`},
		{"CollectionExtract", exprSQL(
			g.CollectionExtract(postgis.KindPoint).Value()), `CAST($2 AS "int4")`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sql, _, err := c.sql()
			if err != nil {
				t.Fatalf("building: %v", err)
			}
			if !strings.Contains(sql, c.want) {
				t.Errorf("the statement does not cast its scalar argument (%s):\n%s", c.want, sql)
			}
		})
	}

	// The geometry operands are cast too, which is what makes PostgreSQL pick
	// the geometry overload rather than the deprecated text one.
	sql, _, err := exprSQL(g.Buffer(1).Value())()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, `CAST($1 AS "geometry")`) {
		t.Errorf("the geometry operand is not cast:\n%s", sql)
	}
	geog, err := postgis.NewPoint(4326, 1, 2).AsGeography()
	if err != nil {
		t.Fatal(err)
	}
	sql, _, err = orm.Compose(nil, orm.Project1(
		postgis.GeogValue[orm.Composed](geog).Area(), func(a float64) float64 { return a },
	)).From(oneRow).SQL()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, `CAST($1 AS "geography")`) {
		t.Errorf("the geography operand is not cast:\n%s", sql)
	}

	// And the distance radius, which distinguishes ST_DWithin's geometry form
	// from its geography form.
	sql, _, err = orm.Compose(nil, orm.Project1(
		orm.Of(orm.BoolOf(g.DWithin(postgis.Value[orm.Composed](postgis.NewPoint(4326, 3, 4)), 5))),
		func(b bool) bool { return b },
	)).From(oneRow).SQL()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, `CAST($3 AS "float8")`) {
		t.Errorf("the DWithin radius is not cast:\n%s", sql)
	}
}
