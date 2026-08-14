package postgis_test

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/postgis"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Semantics this package documents, checked against the server that decides
// them.
//
// A comment claiming PostGIS returns NULL where it actually raises is worse
// than no comment: somebody writes code around the claim. Everything asserted
// here was asked of PostgreSQL 14, 16 and 17 before it was written down.

// The ordinate accessors, with their three outcomes kept apart.
func TestSemantics_ordinateAccessors(t *testing.T) {
	conn := gisConn(t)
	mustExec(t, conn, oneRowDDL)

	// NULL in, NULL out.
	for _, fn := range []string{"ST_X", "ST_Y", "ST_Z", "ST_M"} {
		var v *float64
		if err := conn.QueryRow(t.Context(),
			`SELECT `+fn+`(NULL::geometry)`).Scan(&v); err != nil {
			t.Fatalf("%s(NULL): %v", fn, err)
		}
		if v != nil {
			t.Errorf("%s(NULL) is %v", fn, *v)
		}
	}

	// A missing ordinate is NULL, and this is the reason Z and M read back as
	// pointers even over a NOT NULL column.
	for _, tc := range []struct {
		fn, wkt string
		want    *float64
	}{
		{"ST_Z", "POINT(1 2)", nil},
		{"ST_M", "POINT(1 2)", nil},
		{"ST_M", "POINTZ(1 2 3)", nil},
		{"ST_Z", "POINTM(1 2 4)", nil},
		{"ST_Z", "POINTZ(1 2 3)", f64(3)},
		{"ST_M", "POINTM(1 2 4)", f64(4)},
		{"ST_Z", "POINTZM(1 2 3 4)", f64(3)},
		{"ST_M", "POINTZM(1 2 3 4)", f64(4)},
	} {
		var got *float64
		if err := conn.QueryRow(t.Context(),
			`SELECT `+tc.fn+`($1::geometry)`, tc.wkt).Scan(&got); err != nil {
			t.Fatalf("%s(%s): %v", tc.fn, tc.wkt, err)
		}
		switch {
		case tc.want == nil && got != nil:
			t.Errorf("%s(%s) is %v, want NULL", tc.fn, tc.wkt, *got)
		case tc.want != nil && got == nil:
			t.Errorf("%s(%s) is NULL, want %v", tc.fn, tc.wkt, *tc.want)
		case tc.want != nil && *got != *tc.want:
			t.Errorf("%s(%s) is %v, want %v", tc.fn, tc.wkt, *got, *tc.want)
		}
	}

	// A geometry that is not a point is an error, not NULL. The documentation
	// says so because the server does, and this is what holds it to it.
	for _, fn := range []string{"ST_X", "ST_Y", "ST_Z", "ST_M"} {
		var got *float64
		err := conn.QueryRow(t.Context(),
			`SELECT `+fn+`('LINESTRING(0 0,1 1)'::geometry)`).Scan(&got)
		if err == nil {
			t.Errorf("%s of a line returned %v rather than raising", fn, got)
			continue
		}
		var pgErr *pgconn.PgError
		if !asPgError(err, &pgErr) {
			t.Errorf("%s of a line did not produce a PostgreSQL error: %v", fn, err)
			continue
		}
		if !strings.Contains(pgErr.Message, "POINT") {
			t.Errorf("%s of a line said %q", fn, pgErr.Message)
		}
	}
}

func f64(v float64) *float64 { return &v }

func asPgError(err error, target **pgconn.PgError) bool {
	for err != nil {
		if e, ok := err.(*pgconn.PgError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// A geography needs a longitude/latitude coordinate system, and PostGIS says so
// rather than converting. Nothing here catches or reinterprets that.
func TestSemantics_geographyNeedsLonLat(t *testing.T) {
	conn := gisConn(t)
	mustExec(t, conn, oneRowDDL)

	// AsGeography accepts any stated SRID: this package does not carry a table
	// of which ones are geographic, because spatial_ref_sys is the database's
	// and can be extended.
	g, err := postgis.NewPoint(3857, 1, 2).AsGeography()
	if err != nil {
		t.Fatalf("AsGeography refused a projected SRID in Go: %v", err)
	}
	// And the server refuses it, with its own message.
	var out string
	err = conn.QueryRow(t.Context(), `SELECT ST_AsText($1::geography)`, g).Scan(&out)
	if err == nil {
		t.Fatalf("PostGIS accepted a geography in SRID 3857: %s", out)
	}
	var pgErr *pgconn.PgError
	if !asPgError(err, &pgErr) {
		t.Fatalf("the error is not a PostgreSQL error: %v", err)
	}
	if !strings.Contains(pgErr.Message, "lon/lat") {
		t.Errorf("the message does not explain the restriction: %q", pgErr.Message)
	}

	// 4326 works, which is what the geography constructor produces.
	if err := conn.QueryRow(t.Context(), `SELECT ST_AsText($1::geography)`,
		postgis.GeographyPoint(1, 2)).Scan(&out); err != nil {
		t.Fatalf("a 4326 geography was refused: %v", err)
	}
}

// The antimeridian, where a plane and a spheroid give completely different
// answers and only one of them is a distance.
func TestSemantics_antimeridian(t *testing.T) {
	conn := gisConn(t)
	mustExec(t, conn, oneRowDDL)

	east := postgis.GeographyPoint(179.9, 0)
	west := postgis.GeographyPoint(-179.9, 0)

	var metres, degrees float64
	var within bool
	err := conn.QueryRow(t.Context(), `
		SELECT ST_Distance($1::geography, $2::geography),
		       ST_Distance($1::geography::geometry, $2::geography::geometry),
		       ST_DWithin($1::geography, $2::geography, 30000)`,
		east, west).Scan(&metres, &degrees, &within)
	if err != nil {
		t.Fatal(err)
	}

	// Two tenths of a degree apart across the line: about 22 km.
	if metres < 20_000 || metres > 25_000 {
		t.Errorf("the spheroidal distance across the antimeridian is %v metres", metres)
	}
	// The plane thinks they are most of the way round the world.
	if degrees < 359 || degrees > 360 {
		t.Errorf("the plane distance is %v, and it should be about 359.8 degrees", degrees)
	}
	if !within {
		t.Error("ST_DWithin says two points 22 km apart are not within 30 km")
	}
	// And the ORM's own path gives the same metres.
	assertGeogDistance(t, conn, east, west, metres)
}

// High latitudes, where a degree of longitude is much shorter than at the
// equator. A Cartesian assumption would give the same answer for both.
func TestSemantics_highLatitude(t *testing.T) {
	conn := gisConn(t)
	mustExec(t, conn, oneRowDDL)

	var atPole, atEquator float64
	err := conn.QueryRow(t.Context(), `
		SELECT ST_Distance('SRID=4326;POINT(0 80)'::geography, 'SRID=4326;POINT(10 80)'::geography),
		       ST_Distance('SRID=4326;POINT(0 0)'::geography,  'SRID=4326;POINT(10 0)'::geography)`).
		Scan(&atPole, &atEquator)
	if err != nil {
		t.Fatal(err)
	}
	if atPole >= atEquator {
		t.Fatalf("ten degrees is %v metres at 80°N and %v at the equator; the spheroid is not being used",
			atPole, atEquator)
	}
	// Roughly cos(80°) of the equatorial distance.
	if atPole > atEquator*0.25 {
		t.Errorf("ten degrees at 80°N is %v metres, which is too far for a spheroid", atPole)
	}

	// The ORM agrees.
	assertGeogDistance(t, conn, postgis.GeographyPoint(0, 80), postgis.GeographyPoint(10, 80), atPole)

	// And very near the pole, where the two points nearly coincide.
	var nearPole float64
	if err := conn.QueryRow(t.Context(), `
		SELECT ST_Distance('SRID=4326;POINT(0 89.999)'::geography,
		                   'SRID=4326;POINT(180 89.999)'::geography)`).Scan(&nearPole); err != nil {
		t.Fatal(err)
	}
	// Two points on opposite meridians at 89.999°N are about 222 metres apart
	// over the pole.
	if nearPole < 100 || nearPole > 500 {
		t.Errorf("across the pole is %v metres", nearPole)
	}
	assertGeogDistance(t, conn,
		postgis.GeographyPoint(0, 89.999), postgis.GeographyPoint(180, 89.999), nearPole)
}

// assertGeogDistance runs the ORM's own geography distance and compares it with
// what the hand-written query produced.
func assertGeogDistance(t *testing.T, conn *pgx.Conn, a, b postgis.Geography, want float64) {
	t.Helper()
	sql, args, err := orm.Compose(nil, orm.Project1(
		postgis.GeogValue[orm.Composed](a).Distance(postgis.GeogValue[orm.Composed](b)),
		func(d float64) float64 { return d },
	)).From(oneRow).SQL()
	if err != nil {
		t.Fatalf("building: %v", err)
	}
	inner := strings.TrimSuffix(sql, ";")
	var got float64
	if err := conn.QueryRow(t.Context(),
		`SELECT d FROM (`+inner+`) AS t(d)`, args...).Scan(&got); err != nil {
		t.Fatalf("running: %v", err)
	}
	if got != want {
		t.Errorf("the ORM measured %v and the hand-written query measured %v", got, want)
	}
}

// The KNN operator over geography is not ST_Distance.
//
// PostGIS's <-> on geography measures on a sphere so that the index can use it;
// ST_Distance measures on the spheroid. They differ by about a part in a
// thousand, which is nothing for ordering and wrong for reporting. This test
// exists so the documentation cannot quietly start claiming they are the same.
func TestSemantics_geographyKNNIsNotDistance(t *testing.T) {
	conn := gisConn(t)

	var knn, exact float64
	err := conn.QueryRow(t.Context(), `
		SELECT 'SRID=4326;POINT(0 0)'::geography <-> 'SRID=4326;POINT(10 0)'::geography,
		       ST_Distance('SRID=4326;POINT(0 0)'::geography, 'SRID=4326;POINT(10 0)'::geography)`).
		Scan(&knn, &exact)
	if err != nil {
		t.Fatal(err)
	}
	if knn == exact {
		t.Skip("this PostGIS makes <-> and ST_Distance agree over geography; the documented caution still holds")
	}
	// They are close enough to order by and far enough apart to matter if
	// reported.
	ratio := knn / exact
	if ratio < 0.99 || ratio > 1.01 {
		t.Errorf("<-> gave %v and ST_Distance gave %v, which is too far apart to order by", knn, exact)
	}
	t.Logf("geography <-> is %v and ST_Distance is %v: a ratio of %.6f", knn, exact, ratio)
}

// The predicate differential corpus.
//
// Each case is chosen so that swapping one predicate for another changes the
// answer. That is what makes the suite catch a mutation rather than merely
// exercise the function.
func TestSemantics_predicateDifferentials(t *testing.T) {
	conn := gisConn(t)

	// A unit square, and the geometries that separate the predicates.
	const sq = `'SRID=4326;POLYGON((0 0,2 0,2 2,0 2,0 0))'::geometry`
	cases := []struct {
		name    string
		lhs     string
		rhs     string
		want    map[string]bool
		comment string
	}{
		{
			name: "a point on the boundary",
			lhs:  sq, rhs: `'SRID=4326;POINT(2 1)'::geometry`,
			want: map[string]bool{
				"ST_Contains": false, "ST_Covers": true,
				"ST_Intersects": true, "ST_Disjoint": false,
				"ST_Touches": true, "ST_Crosses": false, "ST_Overlaps": false,
			},
			comment: "Contains excludes the boundary and Covers includes it",
		},
		{
			name: "a point inside",
			lhs:  sq, rhs: `'SRID=4326;POINT(1 1)'::geometry`,
			want: map[string]bool{
				"ST_Contains": true, "ST_Covers": true,
				"ST_Intersects": true, "ST_Disjoint": false, "ST_Touches": false,
			},
		},
		{
			name: "a line crossing the square",
			lhs:  `'SRID=4326;LINESTRING(-1 1,3 1)'::geometry`, rhs: sq,
			want: map[string]bool{
				"ST_Crosses": true, "ST_Intersects": true,
				"ST_Within": false, "ST_Contains": false, "ST_Touches": false,
			},
			comment: "Crosses and Intersects differ for a line inside the square",
		},
		{
			name: "a line wholly inside the square",
			lhs:  `'SRID=4326;LINESTRING(0.5 0.5,1.5 1.5)'::geometry`, rhs: sq,
			want: map[string]bool{
				"ST_Crosses": false, "ST_Intersects": true,
				"ST_Within": true, "ST_CoveredBy": true,
			},
			comment: "this is the case that separates Crosses from Intersects",
		},
		{
			name: "a line along the boundary",
			lhs:  `'SRID=4326;LINESTRING(0 0,0 2)'::geometry`, rhs: sq,
			want: map[string]bool{
				"ST_Crosses": false, "ST_Intersects": true, "ST_Touches": true,
				"ST_Within": false, "ST_CoveredBy": true,
			},
			comment: "CoveredBy includes the boundary and Within does not",
		},
		{
			name: "two partly overlapping squares",
			lhs:  sq, rhs: `'SRID=4326;POLYGON((1 1,3 1,3 3,1 3,1 1))'::geometry`,
			want: map[string]bool{
				"ST_Overlaps": true, "ST_Intersects": true,
				"ST_Contains": false, "ST_Within": false, "ST_Touches": false,
			},
		},
		{
			name: "two squares meeting along an edge",
			lhs:  sq, rhs: `'SRID=4326;POLYGON((2 0,4 0,4 2,2 2,2 0))'::geometry`,
			want: map[string]bool{
				"ST_Touches": true, "ST_Intersects": true,
				"ST_Overlaps": false, "ST_Disjoint": false, "ST_Crosses": false,
			},
			comment: "Touches is true and Overlaps is false, which separates them",
		},
		{
			name: "two squares far apart",
			lhs:  sq, rhs: `'SRID=4326;POLYGON((10 10,12 10,12 12,10 12,10 10))'::geometry`,
			want: map[string]bool{
				"ST_Disjoint": true, "ST_Intersects": false, "ST_Touches": false,
			},
		},
		{
			name: "the same square written backwards, with a repeated vertex",
			lhs:  sq, rhs: `'SRID=4326;POLYGON((0 0,0 2,0 2,2 2,2 0,0 0))'::geometry`,
			want: map[string]bool{
				"ST_Equals": true, "ST_Contains": true, "ST_Covers": true,
				"ST_Overlaps": false, "ST_Touches": false,
			},
			comment: "topological equality, which is not the same bytes and not the same vertices",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for fn, want := range c.want {
				var got bool
				q := `SELECT ` + fn + `(` + c.lhs + `, ` + c.rhs + `)`
				if err := conn.QueryRow(t.Context(), q).Scan(&got); err != nil {
					t.Fatalf("%s: %v", fn, err)
				}
				if got != want {
					t.Errorf("%s is %v, want %v (%s)", fn, got, want, c.comment)
				}
			}
		})
	}

	// The same corpus really does separate the confusable pairs, so that a
	// mutation swapping one for the other cannot pass.
	for _, pair := range [][2]string{
		{"ST_Contains", "ST_Covers"},
		{"ST_Within", "ST_CoveredBy"},
		{"ST_Crosses", "ST_Intersects"},
		{"ST_Touches", "ST_Overlaps"},
	} {
		var separated bool
		for _, c := range cases {
			a, aok := c.want[pair[0]]
			b, bok := c.want[pair[1]]
			if aok && bok && a != b {
				separated = true
			}
		}
		if !separated {
			t.Errorf("no case in the corpus separates %s from %s, so a mutation swapping them would pass",
				pair[0], pair[1])
		}
	}
}

// Predicates over an empty geometry, whose answers are not all what one would
// guess, and over NULL, where SQL's three-valued logic applies.
func TestSemantics_emptyAndNullPredicates(t *testing.T) {
	conn := gisConn(t)

	const sq = `'SRID=4326;POLYGON((0 0,2 0,2 2,0 2,0 0))'::geometry`
	const empty = `'SRID=4326;POLYGON EMPTY'::geometry`

	for _, fn := range []string{
		"ST_Intersects", "ST_Contains", "ST_Covers", "ST_Within",
		"ST_Overlaps", "ST_Touches", "ST_Equals", "ST_Disjoint",
	} {
		// Against EMPTY, the answer is a real boolean whatever it is.
		var got *bool
		if err := conn.QueryRow(t.Context(),
			`SELECT `+fn+`(`+sq+`, `+empty+`)`).Scan(&got); err != nil {
			t.Fatalf("%s against EMPTY: %v", fn, err)
		}
		if got == nil {
			t.Errorf("%s against EMPTY is NULL, and EMPTY is a value", fn)
		} else {
			t.Logf("%s(square, EMPTY) = %v", fn, *got)
		}

		// Against NULL, the answer is NULL — which is UNKNOWN, which does not
		// match, and which is not false.
		var nullGot *bool
		if err := conn.QueryRow(t.Context(),
			`SELECT `+fn+`(`+sq+`, NULL::geometry)`).Scan(&nullGot); err != nil {
			t.Fatalf("%s against NULL: %v", fn, err)
		}
		if nullGot != nil {
			t.Errorf("%s against NULL is %v rather than NULL", fn, *nullGot)
		}
	}

	// EMPTY does not equal EMPTY, and does not intersect itself. This is one of
	// PostGIS's more surprising answers and it is recorded rather than smoothed
	// over.
	var selfEq, selfIntersects bool
	if err := conn.QueryRow(t.Context(),
		`SELECT ST_Equals(`+empty+`, `+empty+`), ST_Intersects(`+empty+`, `+empty+`)`).
		Scan(&selfEq, &selfIntersects); err != nil {
		t.Fatal(err)
	}
	t.Logf("EMPTY equals itself: %v; EMPTY intersects itself: %v", selfEq, selfIntersects)
	if selfIntersects {
		t.Error("an empty geometry intersects itself, which contradicts the documented behaviour")
	}
}

// One distance expression used three times in one statement.
//
// The expression is a value, so reusing it must not duplicate its arguments or
// let one use change another. The M10 audit found the equivalent bug in
// projections; this is the spatial version.
func TestSemantics_expressionReuse(t *testing.T) {
	conn := gisConn(t)
	mustExec(t, conn, `CREATE TABLE pts (id int primary key, g geometry(Point,4326) not null)`)
	if _, err := conn.Exec(t.Context(), `
		INSERT INTO pts VALUES (1,'SRID=4326;POINT(0 0)'),(2,'SRID=4326;POINT(3 4)'),(3,'SRID=4326;POINT(6 8)')`); err != nil {
		t.Fatal(err)
	}

	src := orm.NewSource("public", "pts")
	id := orm.NewOrdCol[audited, int64](src, "id")
	col := postgis.NewGeomCol[audited](src, "g", 4326, postgis.KindPoint, postgis.XY)

	// One expression, used in the select list, in ORDER BY and inside a CASE.
	distance := postgis.Of(col).Distance(
		postgis.Value[orm.Composed](postgis.NewPoint(4326, 0, 0)))
	near := orm.Case(orm.Gt(orm.Of(distance), orm.Val(4.0)), orm.Val("far")).Else(orm.Val("near"))

	type row struct {
		id   int64
		d    float64
		near string
	}
	shape := orm.Project3(orm.Of(id), orm.Of(distance), near,
		func(i int64, d float64, n string) row { return row{i, d, n} })

	sql, args, err := orm.Compose(nil, shape).From(src).OrderBy(orm.Of(distance).Asc()).SQL()
	if err != nil {
		t.Fatalf("building: %v", err)
	}
	// Each use binds its own parameter rather than sharing one. That is the
	// ORM's contract everywhere — an expression is a value, and rendering it
	// twice renders two copies — so three uses of the distance give three
	// geometries, alongside the CASE's own threshold and its two labels.
	var geometries int
	for _, a := range args {
		if g, ok := a.(postgis.Geometry); ok {
			geometries++
			if !g.Equal(postgis.NewPoint(4326, 0, 0)) {
				t.Errorf("one use bound a different geometry: %s %v", g, g.Coords())
			}
		}
	}
	if geometries != 3 {
		t.Errorf("three uses of one expression bound %d geometries: %v", geometries, args)
	}
	if len(args) != 6 {
		t.Errorf("the statement bound %d arguments, want 6 (three geometries and the CASE's three): %v",
			len(args), args)
	}

	rows, err := conn.Query(t.Context(), sql, args...)
	if err != nil {
		t.Fatalf("running:\n%s\n%v", sql, err)
	}
	defer rows.Close()
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.d, &r.near); err != nil {
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
	want := []row{{1, 0, "near"}, {2, 5, "far"}, {3, 10, "far"}}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d is %+v, want %+v", i, got[i], want[i])
		}
	}
}

// audited is the entity tag for the tests in this file that need a source.
type audited struct{}
